#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  verify_release_provenance.sh \
    --bundle <provenance-bundle.sigstore.json> \
    --artifact-dir <release-artifact-directory> \
    --release <vX.Y.Z> \
    --source-digest <40-character-commit-sha> \
    [--trusted-root <trusted_root.jsonl>]

Verifies the signed release provenance bundle with GitHub CLI, then proves
that the verified SLSA statement covers every regular release artifact in the
directory except the bundle itself. The bundle may live inside or outside the
artifact directory.
EOF
}

die() {
  echo "verify_release_provenance.sh: $*" >&2
  exit 1
}

bundle=""
artifact_dir=""
release=""
source_digest=""
trusted_root=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --bundle)
      [[ $# -ge 2 && -n "${2:-}" ]] || die "--bundle requires a value"
      bundle="${2:-}"
      shift 2
      ;;
    --artifact-dir)
      [[ $# -ge 2 && -n "${2:-}" ]] || die "--artifact-dir requires a value"
      artifact_dir="${2:-}"
      shift 2
      ;;
    --release)
      [[ $# -ge 2 && -n "${2:-}" ]] || die "--release requires a value"
      release="${2:-}"
      shift 2
      ;;
    --source-digest)
      [[ $# -ge 2 && -n "${2:-}" ]] || die "--source-digest requires a value"
      source_digest="${2:-}"
      shift 2
      ;;
    --trusted-root)
      [[ $# -ge 2 && -n "${2:-}" ]] || die "--trusted-root requires a value"
      trusted_root="${2:-}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      die "unknown argument: $1"
      ;;
  esac
done

[[ -n "$bundle" ]] || die "--bundle is required"
[[ -f "$bundle" && ! -L "$bundle" ]] || die "bundle must be a regular file: $bundle"
[[ -n "$artifact_dir" ]] || die "--artifact-dir is required"
[[ -d "$artifact_dir" && ! -L "$artifact_dir" ]] ||
  die "artifact directory does not exist: $artifact_dir"
[[ "$release" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.-]+)?$ ]] ||
  die "--release must be a v-prefixed semver tag"
[[ "$source_digest" =~ ^[0-9a-f]{40}$ ]] ||
  die "--source-digest must be a 40-character lowercase commit SHA"
if [[ -n "$trusted_root" ]]; then
  [[ -f "$trusted_root" && ! -L "$trusted_root" ]] ||
    die "trusted root must be a regular file: $trusted_root"
fi
command -v gh >/dev/null 2>&1 || die "gh is required"
command -v python3 >/dev/null 2>&1 || die "python3 is required"

bundle="$(cd "$(dirname "$bundle")" && pwd)/$(basename "$bundle")"
artifact_dir="$(cd "$artifact_dir" && pwd)"
anchor="$artifact_dir/SHA256SUMS.txt"
[[ -f "$anchor" && ! -L "$anchor" ]] ||
  die "artifact directory is missing regular SHA256SUMS.txt"

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/tunnel-client-release-provenance.XXXXXX")"
trap 'rm -rf "$tmp_dir"' EXIT
verified_json="$tmp_dir/verified.json"

verify_args=(
  "$anchor"
  --bundle "$bundle"
  --repo openai/tunnel-client
  --signer-workflow openai/tunnel-client/.github/workflows/release.yml
  --source-ref "refs/tags/$release"
  --source-digest "$source_digest"
  --signer-digest "$source_digest"
  --predicate-type https://slsa.dev/provenance/v1
  --deny-self-hosted-runners
  --format json
)
if [[ -n "$trusted_root" ]]; then
  verify_args+=(--custom-trusted-root "$trusted_root")
fi
gh attestation verify "${verify_args[@]}" >"$verified_json"

python3 - "$artifact_dir" "$bundle" "$verified_json" <<'PY'
import hashlib
import json
import pathlib
import re
import sys

artifact_dir = pathlib.Path(sys.argv[1])
bundle = pathlib.Path(sys.argv[2])
verified_path = pathlib.Path(sys.argv[3])
sha256_re = re.compile(r"^[0-9a-f]{64}$")


def fail(message: str) -> None:
    raise SystemExit(f"verify_release_provenance.sh: {message}")


try:
    verified = json.loads(verified_path.read_text(encoding="utf-8"))
except (OSError, json.JSONDecodeError) as exc:
    fail(f"gh verification output is not valid JSON: {exc}")

if not isinstance(verified, list) or len(verified) != 1:
    fail("gh must return exactly one verified attestation")
result = verified[0]
if not isinstance(result, dict):
    fail("gh verification result must be an object")
verification_result = result.get("verificationResult")
if not isinstance(verification_result, dict):
    fail("gh verification result is missing verificationResult")
statement = verification_result.get("statement")
if not isinstance(statement, dict):
    fail("gh verification result is missing statement")
if statement.get("predicateType") != "https://slsa.dev/provenance/v1":
    fail("verified statement is not SLSA provenance v1")
subjects = statement.get("subject")
if not isinstance(subjects, list) or not subjects:
    fail("verified statement has no subjects")

actual: dict[str, str] = {}
for subject in subjects:
    if not isinstance(subject, dict):
        fail("verified statement contains a malformed subject")
    name = subject.get("name")
    digest = subject.get("digest")
    if (
        not isinstance(name, str)
        or pathlib.PurePosixPath(name).name != name
        or pathlib.PureWindowsPath(name).name != name
    ):
        fail(f"verified statement contains a non-basename subject: {name!r}")
    if not isinstance(digest, dict) or not isinstance(digest.get("sha256"), str):
        fail(f"verified statement subject has no SHA256 digest: {name}")
    sha256 = digest["sha256"]
    if not sha256_re.fullmatch(sha256):
        fail(f"verified statement subject has invalid SHA256 digest: {name}")
    if name in actual:
        fail(f"verified statement contains duplicate subject: {name}")
    actual[name] = sha256

expected: dict[str, str] = {}
for path in sorted(artifact_dir.iterdir(), key=lambda candidate: candidate.name):
    if path.is_symlink() or not path.is_file():
        fail(f"artifact directory must contain only regular root files: {path.name}")
    if path.resolve() == bundle.resolve():
        continue
    expected[path.name] = hashlib.sha256(path.read_bytes()).hexdigest()

if not expected:
    fail("artifact directory contains no attested release artifacts")
if set(actual) != set(expected):
    missing = sorted(set(expected) - set(actual))
    unexpected = sorted(set(actual) - set(expected))
    fail(f"verified subject set differs from release artifacts: missing={missing}, unexpected={unexpected}")
for name, digest in expected.items():
    if actual[name] != digest:
        fail(f"verified SHA256 digest differs from release artifact: {name}")
PY

echo "release provenance bundle: passed"
