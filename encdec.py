from pathlib import Path
import subprocess
import urllib.parse
import httpx

SECURE_JS = Path(__file__).with_name("secure.js")

BOOTSTRAP = """
if (typeof window === "undefined") {
  Object.defineProperty(navigator, "appCodeName", {
    get() {
        return "Mozilla";
    }
});
  globalThis.location = {
    host: "comix.to",
    hostname: "comix.to",
    replace() {
    }
  }
  globalThis.localStorage = {}
  globalThis.window = new Proxy({
    innerWidth: 1024,
    location: globalThis.location,
  }, {
    get(target, prop) {
      if (prop in target) {
        return Reflect.get(target, prop);
      }
      return () => {};
    },
  });
  globalThis.document = new Proxy({
    createElement(tag) {
      return {
        tagName: tag.toUpperCase(),
        setAttribute(name, value) {
        },
        appendChild(child) {
        },
        addEventListener(event, handler) {
        },
        removeEventListener(event, handler) {
        },
        getContext(contextType) {
          return {};
        }
      }
    },
    body: {
      appendChild(child) {
      }
    },
  }, {
    get(target, prop) {
      if (prop in target) {
        return Reflect.get(target, prop);
      }
      return () => {};
    },
  });

  setTimeout(() => {
    let argv = process.argv;
    if (argv[2] === "sign") {
      let path = argv[3];
      console.log(ro(path));
      process.exit(0);
    }
    io({
      interceptors: {
        request: { use: (_) => {} },
        response: { use: (handler) => {
          let resp = JSON.parse(argv[3]);
          const data = {
            headers: {
              "x-enc": "1",
            },
            data: resp,
          }
          console.log(JSON.stringify(handler(data).data))
          process.exit(0);
        } },
      }
    })
  })
}
"""

# init secure.js
print("Patching secure.js...")
secure_js = httpx.get(
    "https://comix.to/assets/build/35595e3de3c99889c1aa70/dist/secure-tfsd33-MxlixIFJ.js"
).text
with open(SECURE_JS, "w") as f:
    f.write(BOOTSTRAP + "\n" + secure_js)

print("Initialization complete.")


def sign(url):
    result = subprocess.run(
        ["node", str(SECURE_JS), "sign", url],
        capture_output=True,
        text=True,
    )
    if result.returncode == 0:
        param = result.stdout.strip()
        if "?" in url:
            return url + "&_=" + param
        else:
            return url + "?_=" + param
    else:
        print(f"Error signing URL: {result.stderr}")
        return ""


def decrypt(data):
    result = subprocess.run(
        ["node", str(SECURE_JS), "decrypt", data],
        capture_output=True,
        text=True,
    )
    if result.returncode == 0:
        return result.stdout.strip()
    else:
        print(f"Error decrypting data: {result.stderr}")
        return None
