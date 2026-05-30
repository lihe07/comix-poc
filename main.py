from encdec import sign, decrypt
import httpx
import asyncio
import parsel
import json

# url = input("Enter the URL to download: ")


def get_chapters(id):
    url = f"https://comix.to/api/v1/manga/2vzj2/chapters?page=1&limit=20&order[number]=desc"
    signed_url = sign(url)
    print(f"Signed URL: {signed_url}")
    resp = httpx.get(signed_url)
    decrypted_data = json.loads(decrypt(resp.text) or "{}")
    print(decrypted_data)


get_chapters("2vzj2")


def download_chapter(chapter_id):
    url = f"https://comix.to/api/v1/chapters/{chapter_id}"
    print("Signing URL...")
    signed_url = sign(url)
    print(f"Signed URL: {signed_url}")

    resp = httpx.get(signed_url)

    if resp.status_code == 200:
        print("Decrypting response...")
        decrypted_data = json.loads(decrypt(resp.text) or "{}")
        items = decrypted_data.get("pages", {}).get("items", [])
        for item in items:
            image_url = item.get("url")
            print(f"Image URL: {image_url}")


download_chapter(
    "8998931"
)  # /title/2vzj2-im-in-love-with-the-villainess/8998931-chapter-61
