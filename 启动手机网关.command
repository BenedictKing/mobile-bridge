#!/bin/zsh
cd -- "${0:A:h}" || exit 1
[[ -x dist/gateway/codex-mobile-gateway ]] || make build-gateway-package || exit 1
exec "${PWD}/dist/gateway/codex-mobile-gateway" serve --data-dir "${PWD}/.local"
