# Comix.to Downloader PoC

A proof of concept for downloading from Comix.to.

Latest updates to `Comix.to` added Pro obfuscator.io to the JavaScript, which contains the rotated decryption function as well as the URL signing function.

This PoC automatically patches the obfuscated `secure-[hash].js` file, and uses Node.js to emulate the decryption and signing functions.

## Usage

Install dependencies with `uv sync`, then download a chapter:

```bash
uv run python main.py "https://comix.to/title/2vzj2-im-in-love-with-the-villainess/8998931-chapter-61"
```

Download the first three English chapters from a manga id using eight page-download threads:

```bash
uv run python main.py 2vzj2 --language en --limit 3 --workers 8
```

Downloaded pages are written under `downloads/` by default. Existing non-empty page files are skipped on repeat runs, and a `.download-complete.json` marker is written after a chapter finishes successfully so future runs can skip the whole chapter. Use `--dry-run` to inspect selected chapters without downloading images, and `--overwrite` to ignore existing pages and completion markers.

A partially deobfuscated version of the `secure-[hash].js` file is included in this repo for reference. See [secure_dec.js](secure_dec.js).

## Rod renderer downloader

`rod_downloader.go` is a separate Go/Rod downloader for reader pages that render pages as either raw `<img>` tags or `<canvas>` tags. It signs/decrypts API metadata in a browser page with the site `ro` and `io` functions, then visits each chapter page and saves raw image URLs directly or captures canvases with `canvas.toDataURL("image/png")`.

```bash
go run ./rod_downloader.go "https://comix.to/title/2vzj2-im-in-love-with-the-villainess/8998931-chapter-61"
```

Rendered output is written under `downloads-rod/` by default. Use `--headful` to watch the browser, `--render-timeout` if canvas pages need longer to paint, and the same target styles as the Python downloader for chapter URLs, chapter ids, manga URLs, and manga ids.
