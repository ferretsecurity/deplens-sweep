#!/usr/bin/env python3
"""Refresh the fixed top-1,000 GitHub OSS corpus using only Python's stdlib.

Run from any directory with GITHUB_TOKEN, GH_TOKEN, or an authenticated gh CLI.
GitHub search docs: https://docs.github.com/en/rest/search/search#search-repositories
SPDX source is pinned below. Its isOsiApproved flag determines eligibility.
"""

import datetime
import hashlib
import json
import math
import os
from pathlib import Path
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


ROOT = Path(__file__).resolve().parent.parent
SPDX_URL = (
    "https://raw.githubusercontent.com/spdx/license-list-data/"
    "ab9b0904b43db6f4b67431efd8821feb821f1c8b/json/licenses.json"
)
BASE_QUERY = "is:public fork:false archived:false"
TARGET = 1000


def fetch_json(url, token=""):
    headers = {"Accept": "application/vnd.github+json", "User-Agent": "deplens-sweep-corpus"}
    request = urllib.request.Request(url, headers=headers)
    if token:
        request.add_unredirected_header("Authorization", "Bearer " + token)
    for attempt in range(7):
        try:
            with urllib.request.urlopen(request, timeout=60) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            if error.code not in (403, 429, 500, 502, 503, 504) or attempt == 6:
                raise RuntimeError(f"HTTP {error.code} retrieving {url}") from None
            retry_after = error.headers.get("Retry-After")
            reset = error.headers.get("X-RateLimit-Reset")
            delay = 2 ** (attempt + 1)
            if retry_after and retry_after.isdigit():
                delay = max(delay, int(retry_after))
            elif error.headers.get("X-RateLimit-Remaining") == "0" and reset:
                delay = max(delay, int(reset) - int(time.time()) + 1)
            if delay > 900:
                raise RuntimeError("GitHub rate limit requires waiting more than 15 minutes; rerun later") from None
        except (urllib.error.URLError, TimeoutError) as error:
            if attempt == 6:
                raise RuntimeError(f"Network request failed for {url}: {error}") from None
            delay = 2 ** (attempt + 1)
        print(f"Retrying request in {delay}s", file=sys.stderr, flush=True)
        time.sleep(delay)
    raise RuntimeError("request retries exhausted")


def collect(token, approved):
    eligible = {}
    seen = set()
    queries = []

    def search(query, page=1):
        params = urllib.parse.urlencode({"q": query, "sort": "stars", "order": "desc", "per_page": 100, "page": page})
        # Authenticated GitHub repository search allows 30 requests per minute.
        time.sleep(2.1)
        for attempt in range(3):
            data = fetch_json("https://api.github.com/search/repositories?" + params, token)
            if data.get("incomplete_results") is False:
                return data
            if attempt < 2:
                time.sleep(2.1)
        raise RuntimeError(f"GitHub returned incomplete results for {query!r}")

    def band(low, high=None):
        if len(eligible) >= TARGET:
            return
        qualifier = f"stars:{low}..{high}" if high is not None else f"stars:>={low}"
        query = BASE_QUERY + " " + qualifier
        first = search(query)
        total = first["total_count"]
        if total > 1000:
            if high is None:
                high = first["items"][0]["stargazers_count"]
            if high == low:
                raise RuntimeError(f"Cannot enumerate {total} repositories in {query!r} below GitHub's search cap")
            middle = (low + high) // 2
            band(middle + 1, high)
            band(low, middle)
            return
        items = list(first["items"])
        for page in range(2, math.ceil(total / 100) + 1):
            data = search(query, page)
            items.extend(data["items"])
        if len(items) != total or len({item["id"] for item in items}) != total:
            raise RuntimeError(f"Search changed during pagination for {query!r}; rerun to obtain a consistent band")
        queries.append((query, total))
        for item in items:
            if item["private"] or item["fork"] or item["archived"]:
                raise RuntimeError(f"GitHub returned an excluded repository: {item['full_name']}")
            stars = item["stargazers_count"]
            if stars < low or (high is not None and stars > high):
                raise RuntimeError("Star counts changed outside the requested band; rerun")
            if item["id"] in seen:
                raise RuntimeError("Repository moved between star bands during collection; rerun")
            seen.add(item["id"])
            license_id = (item.get("license") or {}).get("spdx_id")
            if license_id in approved:
                eligible[item["id"]] = {
                    "id": item["id"], "name": item["full_name"],
                    "stars": stars, "license": license_id,
                }
        print(f"{qualifier}: {total} repositories, {len(eligible)} OSI-approved so far", file=sys.stderr, flush=True)

    band(100000)
    high = 99999
    low = 50000
    while len(eligible) < TARGET:
        band(low, high)
        if low == 0 and len(eligible) < TARGET:
            raise RuntimeError(f"Found only {len(eligible)} eligible repositories")
        high = low - 1
        low //= 2
    selected = sorted(eligible.values(), key=lambda repo: (-repo["stars"], repo["name"].casefold()))[:TARGET]
    return selected, queries, len(seen)


def main():
    if len(sys.argv) != 1:
        raise RuntimeError("Usage: python3 scripts/generate-oss-corpus.py")
    token = os.getenv("GITHUB_TOKEN") or os.getenv("GH_TOKEN")
    if not token:
        try:
            token = subprocess.check_output(["gh", "auth", "token"], text=True, stderr=subprocess.DEVNULL).strip()
        except (FileNotFoundError, subprocess.CalledProcessError):
            raise RuntimeError("Set GITHUB_TOKEN or GH_TOKEN, or authenticate with gh auth login") from None
    started = datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds")
    license_path = ROOT / "internal/sweep/osi-licenses.txt"
    license_bytes = license_path.read_bytes()
    approved = set(license_bytes.decode().splitlines())
    spdx = fetch_json(SPDX_URL)
    official = {license["licenseId"] for license in spdx["licenses"] if license.get("isOsiApproved")}
    if approved != official or license_bytes.decode().splitlines() != sorted(official):
        raise RuntimeError("Checked-in OSI license list differs from the pinned SPDX source")
    selected, queries, examined = collect(token, approved)
    if len(selected) != TARGET or len({repo["name"].casefold() for repo in selected}) != TARGET:
        raise RuntimeError("Corpus must contain exactly 1,000 unique repositories")
    finished = datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds")
    lines = [
        "# Generated by python3 scripts/generate-oss-corpus.py. Regenerating replaces this file.",
        f"# Collection started: {started}", f"# Collection finished: {finished}",
        "# Selection: 1,000 most-starred public, non-fork, non-archived GitHub repositories",
        "# whose GitHub-detected SPDX license has isOsiApproved=true in the pinned SPDX list.",
        "# This is a live search snapshot, not an atomic historical ranking or legal review.",
        "# Repositories with unknown, missing, or non-OSI-approved detected licenses are excluded.",
        "# Equal star counts sort by repository name, case-insensitive.",
        f"# SPDX source: {SPDX_URL}",
        f"# SPDX license list version: {spdx['licenseListVersion']}",
        f"# OSI IDs SHA-256: {hashlib.sha256(license_bytes).hexdigest()}",
        f"# Examined {examined} repositories across complete disjoint star bands:",
    ]
    lines.extend(f"#   {query} -> {count} repositories" for query, count in queries)
    lines.extend([
        "# GitHub search cap is 1,000 per query; larger bands are split before pagination.",
        "# GitHub REST: https://docs.github.com/en/rest/search/search#search-repositories",
        "results_dir: ./oss-run/results", "temp_dir: ./oss-run/checkouts", "workers: 1",
        "timeout: 60m", "min_free_bytes: 1073741824", "review: true", "",
        "connections:", "  - name: github-oss", "    provider: github",
        "    server_url: https://github.com", "    token_env: GITHUB_TOKEN",
        "    include_forks: false", "    include_archived: false",
        "    include:", "      repositories:",
    ])
    lines.extend(
        f"        - {repo['name']} # stars: {repo['stars']}; SPDX: {repo['license']}; GitHub ID: {repo['id']}"
        for repo in selected
    )
    lines.extend(["", "deplens:", "  executable: deplens", ""])
    output = ROOT / "sweep-oss.yaml"
    temporary = output.with_suffix(".yaml.tmp")
    temporary.write_text("\n".join(lines))
    temporary.replace(output)
    print(f"Wrote {output} with {len(selected)} repositories; cutoff {selected[-1]['stars']} stars")


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, OSError, ValueError, KeyError) as error:
        print(f"Corpus generation failed: {error}", file=sys.stderr)
        sys.exit(1)
