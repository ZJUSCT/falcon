#!/usr/bin/env bash
set -euo pipefail
redirect=$(curl -fsS -o /dev/null -H "Host: $E2E_SITE_HOST" \
  -w '%{http_code} %{redirect_url}' \
  "http://$E2E_ENVOY_ADDR/proxy-subset/index.html?check=1")
test "$redirect" = "301 http://$E2E_SITE_HOST/proxy-demo/subset/index.html?check=1"
response=$(curl -fsSL -D - \
  --connect-to "$E2E_SITE_HOST:80:$E2E_ENVOY_ADDR" \
  "http://$E2E_SITE_HOST/proxy-subset/index.html")
[[ "$response" == *UTC* ]]
[[ "${response,,}" == *'x-falcon-e2e-proxy: true'* ]]
curl -fsS -H "Host: $E2E_SITE_HOST" "http://$E2E_ENVOY_ADDR/mirrorz.json" |
  jq -e '
    (.mirrors[] | select(.cname == "proxy-demo")) as $parent |
    (.mirrors[] | select(.cname == "proxy-subset")) |
    .status == $parent.status and (.status | startswith("CN"))
    and .size == null
  '
