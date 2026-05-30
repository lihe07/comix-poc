# Comix.to Downloader PoC

A proof of concept for downloading from Comix.to.

Latest updates to `Comix.to` added Pro obfuscator.io to the JavaScript, which contains the rotated decryption function as well as the URL signing function.

This PoC automatically patches the obfuscated `secure-[hash].js` file, and uses Node.js to emulate the decryption and signing functions.

