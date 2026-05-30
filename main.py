import argparse
import json
import mimetypes
import os
import re
import sys
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import urlparse

import httpx

from encdec import decrypt, ensure_secure_js, sign


API_BASE = "https://comix.to/api/v1"
HEADERS = {
    "User-Agent": (
        "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "
        "(KHTML, like Gecko) Chrome/126.0 Safari/537.36"
    ),
    "Accept": "*/*",
    "Referer": "https://comix.to/",
}
CHUNK_SIZE = 1024 * 256


@dataclass(frozen=True)
class Target:
    kind: str
    identifier: str
    title_slug: str | None = None


@dataclass(frozen=True)
class PageJob:
    index: int
    url: str
    dest: Path


@dataclass(frozen=True)
class PageResult:
    index: int
    status: str
    dest: Path
    bytes_written: int = 0
    error: str | None = None


def parse_args():
    parser = argparse.ArgumentParser(
        description="Download Comix.to chapters using signed/decrypted API responses."
    )
    parser.add_argument(
        "targets",
        nargs="+",
        help=(
            "Chapter URL/id, manga URL, or manga id. Manga ids are the short id at the "
            "start of a title slug, for example 2vzj2."
        ),
    )
    parser.add_argument(
        "-o",
        "--output",
        type=Path,
        default=Path("downloads"),
        help="Directory to write downloaded chapters into.",
    )
    parser.add_argument(
        "-w",
        "--workers",
        type=int,
        default=8,
        help="Number of page download threads per chapter.",
    )
    parser.add_argument(
        "--order",
        choices=("asc", "desc"),
        default="asc",
        help="Chapter order for manga targets.",
    )
    parser.add_argument(
        "--language",
        help="Only download chapters with this language code for manga targets, e.g. en.",
    )
    parser.add_argument(
        "--limit",
        type=int,
        help="For manga targets, download only the first N chapters after ordering/filtering.",
    )
    parser.add_argument(
        "--overwrite",
        action="store_true",
        help="Re-download pages that already exist.",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="Fetch metadata and print what would be downloaded without writing images.",
    )
    parser.add_argument(
        "--retries",
        type=int,
        default=3,
        help="Attempts per page before reporting failure.",
    )
    parser.add_argument(
        "--timeout",
        type=float,
        default=30,
        help="HTTP timeout in seconds.",
    )
    parser.add_argument(
        "--refresh-secure",
        action="store_true",
        help="Re-fetch and patch secure.js before signing/decrypting API calls.",
    )
    return parser.parse_args()


def parse_target(raw):
    raw = raw.strip()
    parsed = urlparse(raw)
    parts = [part for part in parsed.path.split("/") if part]

    if len(parts) >= 2 and parts[0] == "title":
        title_slug = parts[1]
        manga_id = title_slug.split("-", 1)[0]
        if len(parts) >= 3:
            chapter_match = re.match(r"(\d+)", parts[2])
            if not chapter_match:
                raise ValueError(f"Could not parse chapter id from URL/path: {raw}")
            return Target("chapter", chapter_match.group(1), title_slug)
        return Target("manga", manga_id, title_slug)

    if parsed.scheme and parsed.netloc:
        raise ValueError(f"Unsupported target URL: {raw}")

    if raw.isdigit():
        return Target("chapter", raw)

    if re.fullmatch(r"[A-Za-z0-9]+", raw):
        return Target("manga", raw)

    if re.fullmatch(r"[A-Za-z0-9]+(?:-[A-Za-z0-9]+)*", raw):
        return Target("manga", raw.split("-", 1)[0], raw)

    raise ValueError(f"Unsupported target: {raw}")


def signed_json(url, timeout):
    signed_url = sign(url)
    if not signed_url:
        raise RuntimeError(f"Failed to sign URL: {url}")

    with httpx.Client(headers=HEADERS, follow_redirects=True, timeout=timeout) as client:
        resp = client.get(signed_url)
        resp.raise_for_status()

    decrypted = decrypt(resp.text)
    if not decrypted:
        raise RuntimeError(f"Empty decrypted response for: {url}")
    return json.loads(decrypted)


def fetch_manga_chapters(manga_id, order, language, timeout):
    page = 1
    chapters = []

    while True:
        url = (
            f"{API_BASE}/manga/{manga_id}/chapters"
            f"?page={page}&limit=100&order[number]={order}"
        )
        data = signed_json(url, timeout)
        items = data.get("items", [])
        if language:
            items = [item for item in items if item.get("language") == language]
        chapters.extend(items)

        meta = data.get("meta", {})
        if not meta.get("hasNext"):
            break
        page += 1

    return chapters


def fetch_chapter(chapter_id, timeout):
    return signed_json(f"{API_BASE}/chapters/{chapter_id}", timeout)


def safe_name(value, fallback="untitled", max_len=110):
    value = str(value or fallback).strip()
    value = re.sub(r"[^\w .()#&+,'-]+", "_", value)
    value = re.sub(r"\s+", " ", value).strip(" ._")
    return (value or fallback)[:max_len].rstrip(" ._")


def number_label(number):
    if number is None:
        return "chapter"
    if isinstance(number, float) and number.is_integer():
        number = int(number)
    return f"ch-{number}"


def title_slug_from_chapter(chapter, fallback=None):
    chapter_url = chapter.get("url") or ""
    parts = [part for part in chapter_url.split("/") if part]
    if len(parts) >= 2 and parts[0] == "title":
        return parts[1]
    return fallback


def chapter_dir(output, chapter, title_slug=None):
    title_slug = title_slug_from_chapter(chapter, title_slug)
    root = output / safe_name(title_slug, "unknown-title") if title_slug else output
    label = safe_name(
        f"{number_label(chapter.get('number'))}_{chapter.get('id')}_{chapter.get('name')}",
        f"chapter-{chapter.get('id', 'unknown')}",
    )
    return root / label


def extension_for(url, content_type=None):
    path_ext = Path(urlparse(url).path).suffix
    if path_ext:
        return path_ext
    if content_type:
        guessed = mimetypes.guess_extension(content_type.split(";", 1)[0].strip())
        if guessed:
            return guessed
    return ".bin"


def build_page_jobs(chapter, dest_dir):
    items = chapter.get("pages", {}).get("items", [])
    jobs = []
    for idx, item in enumerate(items, start=1):
        image_url = item.get("url")
        if not image_url:
            continue
        ext = extension_for(image_url)
        jobs.append(PageJob(idx, image_url, dest_dir / f"{idx:03d}{ext}"))
    return jobs


def download_page(job, overwrite, timeout, retries):
    if job.dest.exists() and job.dest.stat().st_size > 0 and not overwrite:
        return PageResult(job.index, "skipped", job.dest, job.dest.stat().st_size)

    job.dest.parent.mkdir(parents=True, exist_ok=True)
    tmp_dest = job.dest.with_name(job.dest.name + ".part")
    last_error = None

    for attempt in range(1, retries + 1):
        bytes_written = 0
        try:
            with httpx.stream(
                "GET",
                job.url,
                headers=HEADERS,
                follow_redirects=True,
                timeout=timeout,
            ) as resp:
                resp.raise_for_status()
                content_type = resp.headers.get("content-type", "")
                if content_type and not content_type.startswith("image/"):
                    raise RuntimeError(f"Unexpected content type: {content_type}")

                with tmp_dest.open("wb") as fh:
                    for chunk in resp.iter_bytes(CHUNK_SIZE):
                        if chunk:
                            fh.write(chunk)
                            bytes_written += len(chunk)

            os.replace(tmp_dest, job.dest)
            return PageResult(job.index, "downloaded", job.dest, bytes_written)
        except Exception as exc:  # noqa: BLE001 - keep downloader resilient per page.
            last_error = f"attempt {attempt}/{retries}: {exc}"
            tmp_dest.unlink(missing_ok=True)

    return PageResult(job.index, "failed", job.dest, error=last_error)


def download_chapter(
    chapter_id,
    output,
    title_slug,
    workers,
    overwrite,
    timeout,
    retries,
    dry_run,
):
    chapter = fetch_chapter(chapter_id, timeout)
    dest_dir = chapter_dir(output, chapter, title_slug)
    jobs = build_page_jobs(chapter, dest_dir)

    title = chapter.get("name") or "Untitled"
    number = number_label(chapter.get("number"))
    print(f"{number} ({chapter.get('id')}): {title}")
    print(f"  pages: {len(jobs)}")
    print(f"  output: {dest_dir}")

    if dry_run:
        return 0

    if not jobs:
        print("  no page URLs found")
        return 1

    failures = 0
    completed = 0
    with ThreadPoolExecutor(max_workers=max(1, workers)) as executor:
        futures = [
            executor.submit(download_page, job, overwrite, timeout, retries)
            for job in jobs
        ]
        for future in as_completed(futures):
            result = future.result()
            completed += 1
            if result.status == "failed":
                failures += 1
                print(
                    f"  [{completed}/{len(jobs)}] failed "
                    f"{result.dest.name}: {result.error}"
                )
            else:
                print(f"  [{completed}/{len(jobs)}] {result.status} {result.dest.name}")

    return failures


def run(args):
    if args.workers < 1:
        raise ValueError("--workers must be at least 1")
    if args.retries < 1:
        raise ValueError("--retries must be at least 1")
    if args.limit is not None and args.limit < 1:
        raise ValueError("--limit must be at least 1")

    ensure_secure_js(force=args.refresh_secure)
    total_failures = 0

    for raw_target in args.targets:
        target = parse_target(raw_target)
        if target.kind == "chapter":
            total_failures += download_chapter(
                target.identifier,
                args.output,
                target.title_slug,
                args.workers,
                args.overwrite,
                args.timeout,
                args.retries,
                args.dry_run,
            )
            continue

        chapters = fetch_manga_chapters(
            target.identifier,
            args.order,
            args.language,
            args.timeout,
        )
        if args.limit is not None:
            chapters = chapters[: args.limit]

        print(f"manga {target.identifier}: {len(chapters)} chapters selected")
        for chapter in chapters:
            total_failures += download_chapter(
                str(chapter["id"]),
                args.output,
                target.title_slug,
                args.workers,
                args.overwrite,
                args.timeout,
                args.retries,
                args.dry_run,
            )

    return 1 if total_failures else 0


def main():
    args = parse_args()
    try:
        return run(args)
    except KeyboardInterrupt:
        print("\nInterrupted.", file=sys.stderr)
        return 130
    except Exception as exc:  # noqa: BLE001 - CLI should return a readable error.
        print(f"Error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
