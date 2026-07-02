#!/bin/sh
set -e

# The dashboard's API base URL is a NEXT_PUBLIC_* value, which Next.js inlines
# into the browser bundle at BUILD time. To ship a single image that works on
# any host, the image is built with a placeholder token and this script swaps in
# the real URL from the runtime $API_URL env var before the server starts.
#
# Keep this token in sync with the NEXT_PUBLIC_API_URL ARG default in Dockerfile.
PLACEHOLDER="__RUNTIME_API_URL__"

if [ -z "$API_URL" ]; then
  echo "ERROR: API_URL is required but not set." >&2
  echo "       Set it to the URL where the browser reaches the rota core API," >&2
  echo "       e.g.  docker run -e API_URL=https://rota.example.com:8001 ..." >&2
  exit 1
fi

echo "Configuring dashboard with API_URL=$API_URL"

# Replace the placeholder in the compiled bundle. Node is guaranteed present in
# this image; split/join sidesteps every sed/regex escaping pitfall with URLs.
API_URL="$API_URL" PLACEHOLDER="$PLACEHOLDER" node -e '
  const fs = require("fs"), path = require("path");
  const ph = process.env.PLACEHOLDER, val = process.env.API_URL;
  let count = 0;
  const walk = (dir) => {
    for (const ent of fs.readdirSync(dir, { withFileTypes: true })) {
      const p = path.join(dir, ent.name);
      if (ent.isDirectory()) { walk(p); continue; }
      if (!/\.(js|json|html|css)$/.test(ent.name)) continue;
      const s = fs.readFileSync(p, "utf8");
      if (s.indexOf(ph) === -1) continue;
      fs.writeFileSync(p, s.split(ph).join(val));
      count++;
    }
  };
  for (const d of ["/app/.next", "/app/public"]) { try { walk(d); } catch (e) {} }
  console.log("Injected API_URL into " + count + " bundle file(s).");
'

exec node server.js
