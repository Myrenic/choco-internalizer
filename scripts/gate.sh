#!/usr/bin/env bash
# Promotion gate: staging -> prod.
#
# Both halves of the gate live in Artifact Keeper, deliberately: a manual
# promote from the UI hits the same wall, and neither half can be waved through
# by a CI change.
#   * scan - the registry scans every ingested .nupkg (grype + an OSV/dependency
#            scan) and enforces `default-cve-policy` at promotion time.
#   * age  - promotion rules on choco-internal-stage hold a package for a
#            cooldown (`min_staging_hours`) before release.
# What this script adds is the part the registry cannot know: that the bytes it
# serves are the bytes it advertised, and a report of every decision.
set -uo pipefail

AK_BASE=${AK_BASE:-https://artifacts.tuntelder.com}
STAGE_REPO=${STAGE_REPO:-choco-internal-stage}
PROD_REPO=${PROD_REPO:-choco-internal-prod}
REPORT=${REPORT:-reports/gate.md}
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [[ -z ${AK_TOKEN:-} ]]; then
  echo "AK_TOKEN is empty" >&2
  exit 2
fi

mkdir -p "$(dirname "$REPORT")"
{
  echo "# Promotion gate"
  echo
  echo "Run: $(date -u +'%Y-%m-%dT%H:%M:%SZ')"
  echo
  echo "Stage: \`$STAGE_REPO\` -> prod: \`$PROD_REPO\`. Scan and age are enforced by the registry's promotion policy; this job verifies integrity and reports each decision."
  echo
  echo '| Package | Version | Integrity | Promotion |'
  echo '|---|---|---|---|'
} >"$REPORT"

api() { curl -sS -H "Authorization: Bearer $AK_TOKEN" -H 'Content-Type: application/json' "$@"; }

errors=0
held=0
promoted_count=0
skipped_count=0

# The listing is cursor-paginated (per_page is capped server-side); follow
# `next_cursor` until it stops, or the tail of a large stage is never gated.
cursor=""
: >"$WORK/staged.tsv"
while :; do
  page=$(api "$AK_BASE/api/v1/repositories/$STAGE_REPO/artifacts?limit=100${cursor:+&cursor=$cursor}")
  jq -r '(.items // [])[] | [.id, (.name // ""), (.version // ""), (.checksum_sha256 // ""), (.size_bytes // 0)] | @tsv' <<<"$page" >>"$WORK/staged.tsv"
  [[ $(jq -r '.has_more // false' <<<"$page") == true ]] || break
  cursor=$(jq -r '.next_cursor // empty' <<<"$page")
  [[ -n $cursor ]] || break
done
mapfile -t artifacts < <(sort -u "$WORK/staged.tsv")
if [[ ${#artifacts[@]} -eq 0 ]]; then
  echo "no staged artifacts" >&2
fi

for line in "${artifacts[@]}"; do
  IFS=$'\t' read -r id name version want_sha size <<<"$line"
  [[ -z $name || -z $version ]] && continue
  echo "::group::gate $name $version"

  # Already released? Then there is nothing to decide.
  if [[ $(api -o /dev/null -w '%{http_code}' -I "$AK_BASE/nuget/$PROD_REPO/v2/package/$name/$version") == 200 ]]; then
    printf '| `%s` | %s | - | already in prod |\n' "$name" "$version" >>"$REPORT"
    ((skipped_count++))
    echo "already in prod"
    echo "::endgroup::"
    continue
  fi

  # 1. Integrity: the bytes the registry serves must hash to what it advertises.
  #    A short read or a mismatch is a store defect, never a package decision.
  nupkg="$WORK/$name.$version.nupkg"
  code=$(api -o "$nupkg" -w '%{http_code}' "$AK_BASE/nuget/$STAGE_REPO/v2/package/$name/$version")
  got_sha=$(sha256sum "$nupkg" 2>/dev/null | cut -d' ' -f1)
  if [[ $code != 200 || -z $got_sha || ( -n $want_sha && $got_sha != "$want_sha" ) ]]; then
    printf '| `%s` | %s | ❌ served=%s advertised=%s | withheld |\n' \
      "$name" "$version" "${got_sha:-none}" "${want_sha:-none}" >>"$REPORT"
    echo "::error title=Integrity::$name $version served ${got_sha:-none} but advertised ${want_sha:-none}"
    ((errors++))
    echo "::endgroup::"
    continue
  fi

  # 2. Ask the registry to promote. A refusal is the gate doing its job.
  body=$(api -X POST "$AK_BASE/api/v1/promotion/repositories/$STAGE_REPO/artifacts/$id/promote" \
    -d "$(jq -nc --arg notes "sha256 verified: $got_sha" '{skip_policy_check:false, notes:$notes}')")
  if [[ $(jq -r '.promoted // false' <<<"$body") == true ]]; then
    printf '| `%s` | %s | ✅ | ✅ promoted |\n' "$name" "$version" >>"$REPORT"
    ((promoted_count++))
    echo "promoted"
  else
    reason=$(jq -r '[.gate_results[]? | select(.passed == false) | .reason] | unique | join("; ")' <<<"$body" 2>/dev/null)
    if [[ -z $reason ]]; then
      printf '| `%s` | %s | ✅ | ❌ %s |\n' "$name" "$version" "$(jq -r '.message // "refused"' <<<"$body")" >>"$REPORT"
      ((errors++))
    else
      printf '| `%s` | %s | ✅ | ⏸ %s |\n' "$name" "$version" "$reason" >>"$REPORT"
      ((held++))
      echo "held: $reason"
    fi
  fi
  echo "::endgroup::"
done

{
  echo
  echo "Promoted: $promoted_count, held: $held, already in prod: $skipped_count, errors: $errors"
  if ((promoted_count == 0 && held > 0)); then
    echo
    echo "Everything was held by a scan or cooldown rule - that is the supply-chain gate working, not a failure."
  fi
} >>"$REPORT"

cat "$REPORT"
if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
  cat "$REPORT" >>"$GITHUB_STEP_SUMMARY"
fi

if ((errors > 0)); then exit 1; fi
exit 0
