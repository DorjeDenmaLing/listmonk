#!/bin/sh
# denma: govulncheck for the update and weekly builds (listmonk-infra's CodeBuild
# projects). Fails on a vulnerability the code reaches, unless
# .github/denma-vuln-accepted.txt lists it and it has no fixed version yet.
#   .github/denma-vulncheck.sh        run from the repository's root
set -eu

out=$(mktemp)
trap 'rm -f "$out"' EXIT
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 -format json ./... >"$out"
python3 -I - "$out" "$(dirname "$0")/denma-vuln-accepted.txt" <<'PY'
import json, sys

text, accepted = open(sys.argv[1]).read(), {}
for line in open(sys.argv[2]):
    if line.strip() and not line.startswith("#"):
        vid, _, why = line.strip().partition(" ")
        accepted[vid] = why.strip()
reached, dec, i = {}, json.JSONDecoder(), 0
while i < len(text):
    if text[i].isspace():
        i += 1
        continue
    obj, i = dec.raw_decode(text, i)
    f = obj.get("finding")
    if f and any(frame.get("function") for frame in f.get("trace", [])):
        reached.setdefault(f["osv"], f.get("fixed_version", ""))
bad = 0
for vid, fixed in sorted(reached.items()):
    if vid in accepted and not fixed:
        print(f"accepted, no fix yet: {vid} ({accepted[vid]})")
    else:
        bad += 1
        print(f"reached: {vid}" + (f", fixed in {fixed}" if fixed else ", no fix yet"))
print(f"{len(reached)} reached, {bad} not accepted")
sys.exit(1 if bad else 0)
PY
