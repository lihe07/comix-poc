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

Downloaded pages are written under `downloads/` by default. Use `--dry-run` to inspect selected chapters without downloading images, and `--overwrite` to replace existing page files.

A partially deobfuscated version of the `secure-[hash].js` file is included in this repo for reference. See [secure-dec.js](secure-dec.js).
