package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

const (
	apiBase           = "https://comix.to/api/v1"
	completeMarker    = ".rod-download-complete.json"
	defaultUserAgent  = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"
	canvasNativePatch = `(() => {
  Object.defineProperty(window, "__comixNativeCanvasToDataURL", {
    value: HTMLCanvasElement.prototype.toDataURL,
    configurable: false,
    writable: false
  });
  Object.defineProperty(window, "__comixNativeCanvasGetImageData", {
    value: CanvasRenderingContext2D.prototype.getImageData,
    configurable: false,
    writable: false
  });
})()`
	chapterPageScript = `async (pageNo, pageIndex, settleMs, timeoutMs) => {
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
  const pages = () => Array.from(document.querySelectorAll(".rpage-page"));
  const pageSelector = ".rpage-page[data-page=\"" + pageNo + "\"]";

  const findPage = () => document.querySelector(pageSelector) || pages()[pageIndex] || null;
  const swipers = () => {
    const out = [];
    for (const el of document.querySelectorAll(".swiper, .swiper-container")) {
      if (el.swiper && typeof el.swiper.slideTo === "function") out.push(el.swiper);
    }
    for (const key of Object.keys(window)) {
      try {
        const value = window[key];
        if (value && typeof value.slideTo === "function" && !out.includes(value)) out.push(value);
      } catch (_) {}
    }
    return out;
  };
  // activate only triggers the slide/scroll; it never blocks for a fixed delay
  // so already-loaded image pages can be captured almost immediately. The
  // polling loop below is responsible for waiting until content is ready.
  const activate = () => {
    for (const swiper of swipers()) {
      try {
        swiper.slideTo(pageIndex, 0, false);
      } catch (_) {}
      try {
        if (typeof swiper.slideToLoop === "function") swiper.slideToLoop(pageIndex, 0, false);
      } catch (_) {}
    }
    const el = findPage();
    if (el) {
      el.scrollIntoView({ block: "center", inline: "center" });
      window.dispatchEvent(new Event("scroll"));
    }
    return el;
  };

  const canvasHasVisiblePixels = (canvas) => {
    try {
      const probeSize = 32;
      const probe = document.createElement("canvas");
      probe.width = probeSize;
      probe.height = probeSize;
      const ctx = probe.getContext("2d", { willReadFrequently: true });
      if (!ctx) return { readable: false, visible: false, error: "canvas has no 2d context" };
      ctx.clearRect(0, 0, probeSize, probeSize);
      ctx.drawImage(canvas, 0, 0, probeSize, probeSize);
      const getImageData = window.__comixNativeCanvasGetImageData || CanvasRenderingContext2D.prototype.getImageData;
      const data = getImageData.call(ctx, 0, 0, probeSize, probeSize).data;
      for (let i = 3; i < data.length; i += 4) {
        if (data[i] !== 0) return { readable: true, visible: true };
      }
      return { readable: true, visible: false, error: "canvas snapshot is fully transparent" };
    } catch (err) {
      return {
        readable: false,
        visible: false,
        error: err && err.message ? err.message : String(err)
      };
    }
  };

  const snapshot = async (el) => {
    if (!el) return { page: pageNo, kind: "unknown", error: "page element disappeared" };

    const img = el.querySelector("img.rpage-page__img, img");
    if (img) {
      if (!img.complete || !img.naturalWidth) {
        try {
          await img.decode();
        } catch (_) {}
      }
      const src = img.currentSrc || img.src || img.getAttribute("src") || "";
      if (!src) return { page: pageNo, kind: "image", error: "image has no src" };
      return {
        page: pageNo,
        kind: "image",
        src,
        width: img.naturalWidth || img.width || 0,
        height: img.naturalHeight || img.height || 0
      };
    }

    const canvas = el.querySelector("canvas.rpage-page__img, canvas");
    if (canvas) {
      if (canvas.width > 0 && canvas.height > 0) {
        try {
          const toDataURL = window.__comixNativeCanvasToDataURL || HTMLCanvasElement.prototype.toDataURL;
          const dataURL = toDataURL.call(canvas, "image/png");
          // The scrambled image is painted onto the canvas asynchronously (the
          // descramble can take ~1s after the slide). Keep polling while the
          // canvas is still fully transparent; if pixel reads are blocked, fall
          // back to comparing the PNG data URL against a fresh blank canvas.
          const ref = document.createElement("canvas");
          ref.width = canvas.width;
          ref.height = canvas.height;
          const blankURL = toDataURL.call(ref, "image/png");
          const visible = canvasHasVisiblePixels(canvas);
          if (dataURL && dataURL !== blankURL && (!visible.readable || visible.visible)) {
            return {
              page: pageNo,
              kind: "canvas",
              dataURL,
              width: canvas.width,
              height: canvas.height
            };
          }
          if (visible.readable && !visible.visible) {
            return {
              page: pageNo,
              kind: "canvas",
              error: visible.error || "canvas snapshot is fully transparent"
            };
          }
        } catch (err) {
          return {
            page: pageNo,
            kind: "canvas",
            error: err && err.message ? err.message : String(err)
          };
        }
      }
      return {
        page: pageNo,
        kind: "canvas",
        error: "canvas snapshot is still empty or not descrambled yet"
      };
    }
    return { page: pageNo, kind: "unknown", error: "no image or canvas found" };
  };

  const deadline = Date.now() + timeoutMs;
  const pollMs = 100;
  let last = { page: pageNo, kind: "unknown", error: "no image or canvas found" };

  // Kick off the slide, give the lazy-loader a brief head start (capped well
  // below the old fixed settle), then poll for readiness.
  activate();
  await sleep(Math.min(settleMs, 150));

  while (Date.now() < deadline) {
    const el = activate();
    last = await snapshot(el);
    if (!last.error) return JSON.stringify(last);
    await sleep(pollMs);
  }

  return JSON.stringify(last);
}`
	apiFetchScript = `async (apiURL) => {
  const waitForSecure = async () => {
    const deadline = Date.now() + 30000;
    while (Date.now() < deadline) {
      if (typeof ro === "function" && typeof io === "function") return;
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
    throw new Error("ro/io were not exposed on the page");
  };

  await waitForSecure();

  const sig = ro(apiURL);
  const signedURL = apiURL + (apiURL.includes("?") ? "&" : "?") + "_=" + encodeURIComponent(sig);
  const resp = await fetch(signedURL, {
    credentials: "include",
    headers: { accept: "*/*" }
  });
  const text = await resp.text();
  if (!resp.ok) {
    throw new Error("API " + resp.status + ": " + text.slice(0, 300));
  }

  let encrypted;
  try {
    encrypted = JSON.parse(text);
  } catch (err) {
    throw new Error("API response was not JSON: " + err.message);
  }

  const decrypted = await new Promise((resolve, reject) => {
    try {
      io({
        interceptors: {
          request: { use: () => {} },
          response: {
            use: (handler) => {
              try {
                const out = handler({ headers: { "x-enc": "1" }, data: encrypted });
                resolve(out.data);
              } catch (err) {
                reject(err);
              }
            }
          }
        }
      });
    } catch (err) {
      reject(err);
    }
  });

  return JSON.stringify(decrypted);
}`
)

type config struct {
	output           string
	order            string
	language         string
	limit            int
	overwrite        bool
	dryRun           bool
	headful          bool
	browserPath      string
	timeout          time.Duration
	renderTimeout    time.Duration
	settle           time.Duration
	retries          int
	concurrency      int
	imageConcurrency int
	targets          []string
}

type target struct {
	kind       string
	identifier string
	titleSlug  string
	rawURL     string
}

type chapter struct {
	ID        string
	Name      string
	Number    any
	URL       string
	Language  string
	TitleSlug string
	Pages     []pageInfo
}

// pageInfo is one entry from the chapter API's pages.items[]. Scrambled pages
// (the API marks them with "s": 1) are served pre-scrambled and must be
// descrambled by the reader's canvas; every other page is a plain image whose
// URL can be downloaded directly over HTTP.
type pageInfo struct {
	URL       string
	Width     int
	Height    int
	Scrambled bool
}

type chapterListResponse struct {
	Items []map[string]any `json:"items"`
	Meta  struct {
		HasNext bool `json:"hasNext"`
	} `json:"meta"`
}

type pageSnapshot struct {
	Page    int    `json:"page"`
	Kind    string `json:"kind"`
	Src     string `json:"src"`
	DataURL string `json:"dataURL"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Error   string `json:"error"`
}

type pageFile struct {
	Page int    `json:"page"`
	File string `json:"file"`
	Kind string `json:"kind"`
}

type completionManifest struct {
	ChapterID string     `json:"chapter_id"`
	PageCount int        `json:"page_count"`
	Files     []pageFile `json:"files"`
}

type pageEntry struct {
	Page  int `json:"page"`
	Index int `json:"index"`
}

type pageOutcome struct {
	line   string
	failed bool
	file   *pageFile
}

type apiClient struct {
	page *rod.Page
	mu   sync.Mutex
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := parseFlags()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	browser, cleanup, err := launchBrowser(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	api, err := newAPIClient(browser, cfg.timeout)
	if err != nil {
		return err
	}

	httpClient := newHTTPClient(cfg)

	chapters, err := collectChapters(api, cfg)
	if err != nil {
		return err
	}

	return downloadChapters(api, browser, httpClient, cfg, chapters)
}

// collectChapters resolves every target into the flat list of chapters to
// download. API calls share a single browser tab, so this stays sequential.
func collectChapters(api *apiClient, cfg config) ([]chapter, error) {
	var chapters []chapter
	for _, raw := range cfg.targets {
		tgt, err := parseTarget(raw)
		if err != nil {
			return nil, err
		}

		if tgt.kind == "chapter" {
			ch, err := api.fetchChapter(tgt.identifier)
			if err != nil {
				return nil, err
			}
			if tgt.rawURL != "" {
				ch.URL = tgt.rawURL
			}
			if tgt.titleSlug != "" {
				ch.TitleSlug = tgt.titleSlug
			}
			chapters = append(chapters, ch)
			continue
		}

		chs, err := api.fetchMangaChapters(tgt.identifier, cfg.order, cfg.language)
		if err != nil {
			return nil, err
		}
		if cfg.limit > 0 && len(chs) > cfg.limit {
			chs = chs[:cfg.limit]
		}
		fmt.Printf("manga %s: %d chapters selected\n", tgt.identifier, len(chs))
		for _, ch := range chs {
			if tgt.titleSlug != "" {
				ch.TitleSlug = tgt.titleSlug
			}
			chapters = append(chapters, ch)
		}
	}
	return chapters, nil
}

// downloadChapters processes chapters with a worker pool, each worker driving
// its own browser tab. Per-chapter output is buffered and flushed atomically so
// concurrent logs stay readable.
func downloadChapters(api *apiClient, browser *rod.Browser, client *http.Client, cfg config, chapters []chapter) error {
	concurrency := cfg.concurrency
	if concurrency > len(chapters) {
		concurrency = len(chapters)
	}
	if concurrency < 1 {
		concurrency = 1
	}

	var (
		wg       sync.WaitGroup
		printMu  sync.Mutex
		errMu    sync.Mutex
		failures int64
		firstErr error
	)
	jobs := make(chan chapter)

	worker := func() {
		defer wg.Done()
		// Each worker keeps a single warm tab and navigates it from chapter to
		// chapter. Opening a fresh tab per chapter forced the comix.to SPA to
		// cold-boot every time (re-parse/run all JS, re-bootstrap ro/io, re-init
		// the reader), which is what dominated the between-chapter delay.
		var page *rod.Page
		defer func() {
			if page != nil {
				_ = page.Close()
			}
		}()
		for ch := range jobs {
			var buf bytes.Buffer
			f, err := downloadRenderedChapter(api, browser, &page, client, cfg, ch, &buf)
			printMu.Lock()
			_, _ = os.Stdout.Write(buf.Bytes())
			printMu.Unlock()
			atomic.AddInt64(&failures, int64(f))
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
			}
		}
	}

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go worker()
	}
	for _, ch := range chapters {
		jobs <- ch
	}
	close(jobs)
	wg.Wait()

	if firstErr != nil {
		return firstErr
	}
	if failures > 0 {
		return fmt.Errorf("%d page(s) failed", failures)
	}
	return nil
}

func parseFlags() (config, error) {
	var cfg config
	var timeoutSeconds float64
	var renderSeconds float64
	var settleMS int

	flag.StringVar(&cfg.output, "o", "downloads-rod", "directory to write rendered chapter pages into")
	flag.StringVar(&cfg.output, "output", "downloads-rod", "directory to write rendered chapter pages into")
	flag.StringVar(&cfg.order, "order", "asc", "chapter order for manga targets: asc or desc")
	flag.StringVar(&cfg.language, "language", "", "only download chapters with this language code for manga targets")
	flag.IntVar(&cfg.limit, "limit", 0, "for manga targets, download only the first N chapters")
	flag.BoolVar(&cfg.overwrite, "overwrite", false, "re-download pages that already exist")
	flag.BoolVar(&cfg.dryRun, "dry-run", false, "navigate and print selected pages without writing files")
	flag.BoolVar(&cfg.headful, "headful", false, "show the browser window")
	flag.StringVar(&cfg.browserPath, "browser", "", "path to a Chrome/Chromium executable")
	flag.Float64Var(&timeoutSeconds, "timeout", 120, "browser/API/image operation timeout in seconds")
	flag.Float64Var(&renderSeconds, "render-timeout", 20, "seconds to wait for a canvas snapshot")
	flag.IntVar(&settleMS, "settle-ms", 250, "initial head-start (ms, capped at 150) after bringing a page into view before polling for readiness")
	flag.IntVar(&cfg.retries, "retries", 3, "attempts per raw image download")
	flag.IntVar(&cfg.concurrency, "concurrency", 3, "number of chapters to download in parallel")
	flag.IntVar(&cfg.imageConcurrency, "image-concurrency", 4, "number of page downloads to run in parallel within a chapter")
	flag.Parse()

	cfg.targets = flag.Args()
	if len(cfg.targets) == 0 {
		return cfg, errors.New("at least one chapter URL/id, manga URL, or manga id is required")
	}
	if cfg.order != "asc" && cfg.order != "desc" {
		return cfg, errors.New("--order must be asc or desc")
	}
	if cfg.limit < 0 {
		return cfg, errors.New("--limit must be non-negative")
	}
	if cfg.retries < 1 {
		return cfg, errors.New("--retries must be at least 1")
	}
	if cfg.concurrency < 1 {
		return cfg, errors.New("--concurrency must be at least 1")
	}
	if cfg.imageConcurrency < 1 {
		return cfg, errors.New("--image-concurrency must be at least 1")
	}
	if timeoutSeconds <= 0 || renderSeconds <= 0 || settleMS < 0 {
		return cfg, errors.New("timeouts must be positive and --settle-ms must be non-negative")
	}

	cfg.timeout = time.Duration(timeoutSeconds * float64(time.Second))
	cfg.renderTimeout = time.Duration(renderSeconds * float64(time.Second))
	cfg.settle = time.Duration(settleMS) * time.Millisecond
	return cfg, nil
}

// newHTTPClient builds an image-download client whose connection pool is sized
// for the configured parallelism. The default transport only keeps 2 idle
// connections per host, which forces constant TLS re-handshakes (and spurious
// timeouts) once many page downloads run concurrently.
func newHTTPClient(cfg config) *http.Client {
	maxConns := cfg.concurrency*cfg.imageConcurrency + 4
	if maxConns < 8 {
		maxConns = 8
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          maxConns * 2,
		MaxIdleConnsPerHost:   maxConns,
		MaxConnsPerHost:       maxConns,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Timeout: cfg.timeout, Transport: transport}
}

func launchBrowser(ctx context.Context, cfg config) (*rod.Browser, func(), error) {
	l := launcher.New().
		Context(ctx).
		Headless(!cfg.headful).
		Set("disable-gpu").
		Set("no-sandbox")
	if cfg.browserPath != "" {
		l = l.Bin(cfg.browserPath)
	}

	controlURL, err := l.Launch()
	if err != nil {
		return nil, nil, err
	}

	browser := rod.New().ControlURL(controlURL).Context(ctx)
	if err := browser.Connect(); err != nil {
		l.Cleanup()
		return nil, nil, err
	}

	cleanup := func() {
		_ = browser.Close()
		l.Cleanup()
	}
	return browser, cleanup, nil
}

func newAPIClient(browser *rod.Browser, timeout time.Duration) (*apiClient, error) {
	page, err := browser.Page(proto.TargetCreateTarget{URL: "https://comix.to/"})
	if err != nil {
		return nil, err
	}
	if err := page.Timeout(timeout).WaitLoad(); err != nil {
		_ = page.Close()
		return nil, err
	}
	if _, err := page.Timeout(timeout).Eval(`() => new Promise((resolve, reject) => {
  const deadline = Date.now() + 30000;
  const tick = () => {
    if (typeof ro === "function" && typeof io === "function") return resolve(true);
    if (Date.now() > deadline) return reject(new Error("timed out waiting for ro/io"));
    setTimeout(tick, 100);
  };
  tick();
})`); err != nil {
		_ = page.Close()
		return nil, err
	}
	return &apiClient{page: page}, nil
}

// signedJSON drives the shared API tab, so calls are serialized to stay safe
// when multiple download workers fetch chapter page lists concurrently.
func (a *apiClient) signedJSON(apiURL string, out any) error {
	a.mu.Lock()
	res, err := a.page.Eval(apiFetchScript, apiURL)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(res.Value.Str()), out)
}

// fetchChapterPages loads just the page list for a chapter (used for manga
// targets where the chapter listing does not embed pages).
func (a *apiClient) fetchChapterPages(chapterID string) ([]pageInfo, error) {
	var payload map[string]any
	if err := a.signedJSON(fmt.Sprintf("%s/chapters/%s", apiBase, url.PathEscape(chapterID)), &payload); err != nil {
		return nil, err
	}
	return pagesFromMap(payload["pages"]), nil
}

func (a *apiClient) fetchMangaChapters(mangaID, order, language string) ([]chapter, error) {
	var chapters []chapter
	for pageNo := 1; ; pageNo++ {
		apiURL := fmt.Sprintf("%s/manga/%s/chapters?page=%d&limit=100&order[number]=%s", apiBase, url.PathEscape(mangaID), pageNo, order)
		var payload chapterListResponse
		if err := a.signedJSON(apiURL, &payload); err != nil {
			return nil, err
		}
		for _, item := range payload.Items {
			ch := chapterFromMap(item)
			if language != "" && ch.Language != language {
				continue
			}
			chapters = append(chapters, ch)
		}
		if !payload.Meta.HasNext {
			break
		}
	}
	return dedupeChapters(chapters), nil
}

// dedupeChapters collapses chapters that share the same chapter number. comix.to
// lists every translation of a chapter as a separate entry under the same
// number, which would otherwise download the same chapter multiple times. The
// listing order is preserved; when page counts are known (embedded in the
// listing) the variant with more pages wins, otherwise the first seen is kept.
func dedupeChapters(chapters []chapter) []chapter {
	seen := make(map[string]int, len(chapters))
	out := make([]chapter, 0, len(chapters))
	for _, ch := range chapters {
		key := chapterNumberKey(ch)
		if idx, ok := seen[key]; ok {
			if len(ch.Pages) > len(out[idx].Pages) {
				out[idx] = ch
			}
			continue
		}
		seen[key] = len(out)
		out = append(out, ch)
	}
	return out
}

// chapterNumberKey identifies chapters that should be treated as duplicates.
// Chapters without a usable number, or bucketed under number 0 (comix.to files
// volumes/specials/previews there, which are distinct content rather than
// translations of one another), fall back to their unique id so they are never
// collapsed together.
func chapterNumberKey(ch chapter) string {
	label := numberLabel(ch.Number)
	if label == "chapter" || label == "ch-0" {
		return "id:" + ch.ID
	}
	return label
}

func (a *apiClient) fetchChapter(chapterID string) (chapter, error) {
	var payload map[string]any
	if err := a.signedJSON(fmt.Sprintf("%s/chapters/%s", apiBase, url.PathEscape(chapterID)), &payload); err != nil {
		return chapter{}, err
	}
	return chapterFromMap(payload), nil
}

func downloadRenderedChapter(api *apiClient, browser *rod.Browser, pagePtr **rod.Page, client *http.Client, cfg config, ch chapter, out io.Writer) (int, error) {
	chapterURL := absoluteComixURL(ch.URL)
	if chapterURL == "" {
		return 0, fmt.Errorf("chapter %s has no URL", ch.ID)
	}
	destDir := chapterDir(cfg.output, ch)

	fmt.Fprintf(out, "%s (%s): %s\n", numberLabel(ch.Number), fallback(ch.ID, "unknown"), fallback(ch.Name, "Untitled"))
	fmt.Fprintf(out, "  url: %s\n", chapterURL)
	fmt.Fprintf(out, "  output: %s\n", destDir)

	// Fast path: a finished chapter with a valid completion marker does not need
	// to fetch metadata or render anything.
	if !cfg.overwrite && !cfg.dryRun && fileExists(filepath.Join(destDir, completeMarker)) {
		if completeMarkerValid(destDir) {
			fmt.Fprintln(out, "  already complete, skipping")
			return 0, nil
		}
		fmt.Fprintln(out, "  completion marker has invalid files, repairing")
	}

	// Page URLs (and the scramble flag) come straight from the chapter API.
	// Chapter targets already carry them; manga-derived chapters fetch lazily.
	if len(ch.Pages) == 0 {
		pages, err := api.fetchChapterPages(ch.ID)
		if err != nil {
			return 0, fmt.Errorf("fetch pages for chapter %s: %w", ch.ID, err)
		}
		ch.Pages = pages
	}

	total := len(ch.Pages)
	scrambled := 0
	for _, p := range ch.Pages {
		if p.Scrambled {
			scrambled++
		}
	}
	fmt.Fprintf(out, "  pages: %d (%d direct, %d rendered)\n", total, total-scrambled, scrambled)
	if cfg.dryRun {
		return 0, nil
	}
	if total == 0 {
		fmt.Fprintln(out, "  no page URLs found")
		return 1, nil
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return 0, err
	}

	results := make([]pageOutcome, total)

	// Partition pages into ones we can pull directly over HTTP and the scrambled
	// ones that still need the in-browser canvas. Pages already on disk are
	// skipped here so neither path pays for them.
	var directIdx, renderIdx []int
	for i, p := range ch.Pages {
		pageNo := i + 1
		if !cfg.overwrite {
			if name, ok := existingPageFile(destDir, pageNo); ok {
				pf := pageFile{Page: pageNo, File: name, Kind: kindFromExt(name)}
				results[i] = pageOutcome{file: &pf, line: fmt.Sprintf("  [%d/%d] skipped %s", pageNo, total, name)}
				continue
			}
		}
		if p.Scrambled {
			renderIdx = append(renderIdx, i)
		} else {
			directIdx = append(directIdx, i)
		}
	}

	// Direct image downloads run concurrently over HTTP and need no browser.
	var wg sync.WaitGroup
	jobs := make(chan int)
	workers := cfg.imageConcurrency
	if workers < 1 {
		workers = 1
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				snap := pageSnapshot{Page: i + 1, Kind: "image", Src: ch.Pages[i].URL}
				results[i] = downloadSnapshot(client, cfg, destDir, snap, i, total)
			}
		}()
	}
	go func() {
		for _, i := range directIdx {
			jobs <- i
		}
		close(jobs)
	}()

	// Only spin up the reader when there is actually something to descramble;
	// this runs in parallel with the direct downloads above.
	var renderErr error
	if len(renderIdx) > 0 {
		renderErr = renderScrambledPages(browser, pagePtr, client, cfg, chapterURL, destDir, ch, renderIdx, total, results)
	}
	wg.Wait()
	if renderErr != nil {
		return 0, renderErr
	}

	failures := 0
	files := make([]pageFile, 0, total)
	for _, r := range results {
		if r.line != "" {
			fmt.Fprintln(out, r.line)
		}
		if r.failed {
			failures++
		}
		if r.file != nil {
			files = append(files, *r.file)
		}
	}

	if failures == 0 {
		if err := writeCompletionMarker(destDir, ch, files); err != nil {
			return failures, err
		}
	}
	return failures, nil
}

// renderScrambledPages boots (or reuses) the worker's warm reader tab and
// captures only the scrambled pages via canvas. Results for the given indices
// are written into results; other indices are left untouched.
func renderScrambledPages(browser *rod.Browser, pagePtr **rod.Page, client *http.Client, cfg config, chapterURL, destDir string, ch chapter, idxs []int, total int, results []pageOutcome) error {
	// Reuse the worker's warm tab when available; only create one on first use.
	// EvalOnNewDocument re-runs on every navigation, so the canvas patch only
	// needs to be installed once for the lifetime of the tab.
	page := *pagePtr
	if page == nil {
		p, err := browser.Page(proto.TargetCreateTarget{})
		if err != nil {
			return err
		}
		if _, err := p.EvalOnNewDocument(canvasNativePatch); err != nil {
			_ = p.Close()
			return err
		}
		page = p
		*pagePtr = page
	}

	if err := page.Navigate(chapterURL); err != nil {
		return err
	}
	if err := page.Timeout(cfg.timeout).WaitLoad(); err != nil {
		return err
	}
	if _, err := page.Timeout(cfg.timeout).Element(".rpage-page"); err != nil {
		return err
	}

	for _, i := range idxs {
		pageNo := i + 1
		entry := pageEntry{Page: pageNo, Index: i}
		snap, err := snapshotPage(page, entry, cfg)
		if snap.Page == 0 {
			snap.Page = pageNo
		}
		if err != nil {
			results[i] = pageOutcome{failed: true, line: fmt.Sprintf("  [%d/%d] failed page %03d: %v", pageNo, total, snap.Page, err)}
			continue
		}
		if snap.Error != "" {
			results[i] = pageOutcome{failed: true, line: fmt.Sprintf("  [%d/%d] failed page %03d: %s", pageNo, total, snap.Page, snap.Error)}
			continue
		}
		results[i] = downloadSnapshot(client, cfg, destDir, snap, i, total)
	}
	return nil
}

// downloadSnapshot persists a single rendered page (raw image or canvas data
// URL). It is safe to call concurrently from multiple workers.
func downloadSnapshot(client *http.Client, cfg config, destDir string, snap pageSnapshot, i, total int) pageOutcome {
	switch snap.Kind {
	case "image":
		ext := extensionForURL(snap.Src, ".img")
		dest := filepath.Join(destDir, fmt.Sprintf("%03d%s", snap.Page, ext))
		status, err := downloadImage(client, snap.Src, dest, cfg)
		if err != nil {
			return pageOutcome{failed: true, line: fmt.Sprintf("  [%d/%d] failed page %03d: %v", i+1, total, snap.Page, err)}
		}
		pf := pageFile{Page: snap.Page, File: filepath.Base(dest), Kind: snap.Kind}
		return pageOutcome{file: &pf, line: fmt.Sprintf("  [%d/%d] %s %s", i+1, total, status, filepath.Base(dest))}
	case "canvas":
		dest := filepath.Join(destDir, fmt.Sprintf("%03d.png", snap.Page))
		status, err := writeDataURL(dest, snap.DataURL, cfg.overwrite)
		if err != nil {
			return pageOutcome{failed: true, line: fmt.Sprintf("  [%d/%d] failed page %03d: %v", i+1, total, snap.Page, err)}
		}
		pf := pageFile{Page: snap.Page, File: filepath.Base(dest), Kind: snap.Kind}
		return pageOutcome{file: &pf, line: fmt.Sprintf("  [%d/%d] %s %s", i+1, total, status, filepath.Base(dest))}
	default:
		return pageOutcome{failed: true, line: fmt.Sprintf("  [%d/%d] failed page %03d: unsupported page kind %q", i+1, total, snap.Page, snap.Kind)}
	}
}

func snapshotPage(page *rod.Page, entry pageEntry, cfg config) (pageSnapshot, error) {
	// A single eval can fail with "context deadline exceeded" when the shared
	// browser is momentarily saturated. Retry instead of failing the page.
	var lastErr error
	for attempt := 1; attempt <= cfg.retries; attempt++ {
		res, err := page.Timeout(cfg.renderTimeout+cfg.settle+5*time.Second).Eval(
			chapterPageScript,
			entry.Page,
			entry.Index,
			int(cfg.settle/time.Millisecond),
			int(cfg.renderTimeout/time.Millisecond),
		)
		if err != nil {
			lastErr = err
			continue
		}
		var snap pageSnapshot
		if err := json.Unmarshal([]byte(res.Value.Str()), &snap); err != nil {
			lastErr = err
			continue
		}
		if snap.Kind == "canvas" && snap.Error == "" {
			if err := validateCanvasDataURL(snap.DataURL); err != nil {
				lastErr = err
				continue
			}
		}
		return snap, nil
	}
	return pageSnapshot{}, lastErr
}

func downloadImage(client *http.Client, imageURL, dest string, cfg config) (string, error) {
	if fileExists(dest) && !cfg.overwrite {
		return "skipped", nil
	}

	var lastErr error
	for attempt := 1; attempt <= cfg.retries; attempt++ {
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return "", err
		}
		tmp := dest + ".part"
		_ = os.Remove(tmp)

		ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
		if err != nil {
			cancel()
			return "", err
		}
		req.Header.Set("User-Agent", defaultUserAgent)
		req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
		req.Header.Set("Referer", "https://comix.to/")

		resp, err := client.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		func() {
			defer cancel()
			defer resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
				return
			}
			if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "image/") {
				lastErr = fmt.Errorf("unexpected content type %q", ct)
				return
			}

			fh, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				lastErr = err
				return
			}
			_, copyErr := io.Copy(fh, resp.Body)
			closeErr := fh.Close()
			if copyErr != nil {
				lastErr = copyErr
				_ = os.Remove(tmp)
				return
			}
			if closeErr != nil {
				lastErr = closeErr
				_ = os.Remove(tmp)
				return
			}
			lastErr = os.Rename(tmp, dest)
		}()
		if lastErr == nil {
			return "downloaded", nil
		}
		_ = os.Remove(tmp)
	}
	return "", lastErr
}

func writeDataURL(dest, dataURL string, overwrite bool) (string, error) {
	if fileExists(dest) && !overwrite && validCanvasPNGFile(dest) {
		return "skipped", nil
	}
	raw, err := decodeCanvasDataURL(dataURL)
	if err != nil {
		return "", err
	}
	if err := validateCanvasPNG(raw); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	tmp := dest + ".part"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return "captured", nil
}

func decodeCanvasDataURL(dataURL string) ([]byte, error) {
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(dataURL, prefix) {
		return nil, errors.New("canvas snapshot was not a PNG data URL")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(dataURL, prefix))
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func validateCanvasDataURL(dataURL string) error {
	raw, err := decodeCanvasDataURL(dataURL)
	if err != nil {
		return err
	}
	return validateCanvasPNG(raw)
}

func validateCanvasPNG(raw []byte) error {
	img, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	return validateCanvasImage(img, format)
}

func validateCanvasImage(img image.Image, format string) error {
	if format != "png" {
		return errors.New("canvas snapshot was not a PNG image")
	}
	bounds := img.Bounds()
	if bounds.Dx() <= 1 || bounds.Dy() <= 1 {
		return fmt.Errorf("canvas snapshot is %dx%d", bounds.Dx(), bounds.Dy())
	}
	if imageFullyTransparent(img) {
		return errors.New("canvas snapshot is fully transparent")
	}
	return nil
}

func imageFullyTransparent(img image.Image) bool {
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, a := img.At(x, y).RGBA()
			if a != 0 {
				return false
			}
		}
	}
	return true
}

func validCanvasPNGFile(filename string) bool {
	fh, err := os.Open(filename)
	if err != nil {
		return false
	}
	defer fh.Close()

	img, format, err := image.Decode(fh)
	return err == nil && validateCanvasImage(img, format) == nil
}

func parseTarget(raw string) (target, error) {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil {
		return target{}, err
	}
	parts := splitPath(parsed.Path)

	if len(parts) >= 2 && parts[0] == "title" {
		titleSlug := parts[1]
		mangaID := strings.SplitN(titleSlug, "-", 2)[0]
		if len(parts) >= 3 {
			id := leadingDigits(parts[2])
			if id == "" {
				return target{}, fmt.Errorf("could not parse chapter id from %q", raw)
			}
			return target{kind: "chapter", identifier: id, titleSlug: titleSlug, rawURL: absoluteComixURL(raw)}, nil
		}
		return target{kind: "manga", identifier: mangaID, titleSlug: titleSlug}, nil
	}

	if parsed.Scheme != "" && parsed.Host != "" {
		return target{}, fmt.Errorf("unsupported target URL: %s", raw)
	}
	if regexp.MustCompile(`^\d+$`).MatchString(raw) {
		return target{kind: "chapter", identifier: raw}, nil
	}
	if regexp.MustCompile(`^[A-Za-z0-9]+$`).MatchString(raw) {
		return target{kind: "manga", identifier: raw}, nil
	}
	if regexp.MustCompile(`^[A-Za-z0-9]+(?:-[A-Za-z0-9]+)*$`).MatchString(raw) {
		return target{kind: "manga", identifier: strings.SplitN(raw, "-", 2)[0], titleSlug: raw}, nil
	}
	return target{}, fmt.Errorf("unsupported target: %s", raw)
}

func chapterFromMap(item map[string]any) chapter {
	ch := chapter{
		ID:       stringValue(item["id"]),
		Name:     stringValue(item["name"]),
		Number:   item["number"],
		URL:      stringValue(item["url"]),
		Language: stringValue(item["language"]),
		Pages:    pagesFromMap(item["pages"]),
	}
	ch.TitleSlug = titleSlugFromURL(ch.URL)
	return ch
}

func pagesFromMap(value any) []pageInfo {
	container, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	rawItems, ok := container["items"].([]any)
	if !ok {
		return nil
	}
	pages := make([]pageInfo, 0, len(rawItems))
	for _, raw := range rawItems {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		pageURL := stringValue(item["url"])
		if pageURL == "" {
			continue
		}
		pages = append(pages, pageInfo{
			URL:       pageURL,
			Width:     intValue(item["width"]),
			Height:    intValue(item["height"]),
			Scrambled: intValue(item["s"]) == 1,
		})
	}
	return pages
}

func intValue(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

func chapterDir(output string, ch chapter) string {
	titleSlug := ch.TitleSlug
	if titleSlug == "" {
		titleSlug = titleSlugFromURL(ch.URL)
	}
	root := output
	if titleSlug != "" {
		root = filepath.Join(output, safeName(titleSlug, "unknown-title", 110))
	}
	label := safeName(fmt.Sprintf("%s_%s_%s", numberLabel(ch.Number), ch.ID, ch.Name), "chapter-"+fallback(ch.ID, "unknown"), 140)
	return filepath.Join(root, label)
}

func writeCompletionMarker(destDir string, ch chapter, files []pageFile) error {
	sort.Slice(files, func(i, j int) bool { return files[i].Page < files[j].Page })
	manifest := completionManifest{
		ChapterID: ch.ID,
		PageCount: len(files),
		Files:     files,
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := filepath.Join(destDir, completeMarker+".part")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(destDir, completeMarker))
}

func completeMarkerValid(destDir string) bool {
	raw, err := os.ReadFile(filepath.Join(destDir, completeMarker))
	if err != nil {
		return false
	}
	var manifest completionManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return false
	}
	if len(manifest.Files) == 0 {
		return false
	}
	if manifest.PageCount != 0 && manifest.PageCount != len(manifest.Files) {
		return false
	}
	for _, file := range manifest.Files {
		if !completionFileValid(destDir, file) {
			return false
		}
	}
	return true
}

func completionFileValid(destDir string, file pageFile) bool {
	if file.Page <= 0 || file.File == "" || filepath.IsAbs(file.File) || file.File != filepath.Base(file.File) {
		return false
	}
	filename := filepath.Join(destDir, file.File)
	if strings.EqualFold(file.Kind, "canvas") || (file.Kind == "" && strings.EqualFold(filepath.Ext(file.File), ".png")) {
		return validCanvasPNGFile(filename)
	}
	return fileExists(filename)
}

func extensionForURL(rawURL, fallbackExt string) string {
	parsed, err := url.Parse(rawURL)
	if err == nil {
		if ext := path.Ext(parsed.Path); ext != "" {
			return ext
		}
	}
	if err == nil {
		if ct := parsed.Query().Get("content-type"); ct != "" {
			if ext, mimeErr := mime.ExtensionsByType(ct); mimeErr == nil && len(ext) > 0 {
				return ext[0]
			}
		}
	}
	return fallbackExt
}

func absoluteComixURL(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return raw
	}
	if strings.HasPrefix(raw, "/") {
		return "https://comix.to" + raw
	}
	return "https://comix.to/" + raw
}

func titleSlugFromURL(raw string) string {
	parts := splitPath(raw)
	if len(parts) >= 2 && parts[0] == "title" {
		return parts[1]
	}
	return ""
}

func splitPath(raw string) []string {
	if parsed, err := url.Parse(raw); err == nil {
		raw = parsed.Path
	}
	raw = strings.Trim(raw, "/")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "/")
}

func leadingDigits(value string) string {
	for i, r := range value {
		if r < '0' || r > '9' {
			return value[:i]
		}
	}
	return value
}

func stringValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case json.Number:
		return v.String()
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func numberLabel(value any) string {
	switch v := value.(type) {
	case nil:
		return "chapter"
	case string:
		if strings.TrimSpace(v) == "" {
			return "chapter"
		}
		return "ch-" + v
	case float64:
		if v == float64(int64(v)) {
			return fmt.Sprintf("ch-%d", int64(v))
		}
		return "ch-" + strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		return "ch-" + v.String()
	default:
		return "ch-" + fmt.Sprint(v)
	}
}

func safeName(value, fallback string, maxLen int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	value = regexp.MustCompile(`[^\w .()#&+,'-]+`).ReplaceAllString(value, "_")
	value = regexp.MustCompile(`\s+`).ReplaceAllString(value, " ")
	value = strings.Trim(value, " ._")
	if value == "" {
		value = fallback
	}
	if len(value) > maxLen {
		value = strings.TrimRight(value[:maxLen], " ._")
	}
	return value
}

func fallback(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	return err == nil && info.Size() > 0
}

// existingPageFile reports whether a non-empty output file for the given page
// number already exists (any extension), returning its base name.
func existingPageFile(destDir string, page int) (string, bool) {
	matches, err := filepath.Glob(filepath.Join(destDir, fmt.Sprintf("%03d.*", page)))
	if err != nil {
		return "", false
	}
	for _, m := range matches {
		if strings.HasSuffix(m, ".part") {
			continue
		}
		if strings.EqualFold(filepath.Ext(m), ".png") {
			if validCanvasPNGFile(m) {
				return filepath.Base(m), true
			}
			continue
		}
		if fileExists(m) {
			return filepath.Base(m), true
		}
	}
	return "", false
}

func kindFromExt(name string) string {
	if strings.EqualFold(filepath.Ext(name), ".png") {
		return "canvas"
	}
	return "image"
}
