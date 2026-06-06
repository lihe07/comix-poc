# Comix.to Downloader PoC

A proof of concept for downloading from Comix.to.

Latest updates to `Comix.to` added Pro obfuscator.io to the JavaScript, which contains the rotated decryption function as well as the URL signing function.

This PoC automatically patches the obfuscated `secure-[hash].js` file, and uses Node.js to emulate the decryption and signing functions.

## Usage (Go)

The Go version of the script can download and unscramble images, but it requires the dependencies in order to run a headless browser.

```bash
go run rod_downloader.go "https://comix.to/title/2vzj2-im-in-love-with-the-villainess"
```

## Usage (Python)

*Python version of the script DOES NOT unscramble the images.*

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

## Duplicated Chapters

By default, the script skips chapters with duplicate indices, (e.g. only one translation of Chapter 1 will be downloaded).

However, the source sometimes label different translations of the same chapter with different indices (e.g. 1 and 1.5). In these cases, the script will download both chapters by default.
