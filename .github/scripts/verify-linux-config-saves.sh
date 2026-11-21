#!/usr/bin/env bash
set -euo pipefail

if [[ $EUID -eq 0 ]]; then
  echo 'Configuration permission checks require a non-root test user.' >&2
  exit 1
fi

scratch="$(mktemp -d)"
mounted=""
cleanup() {
  if [[ -n "$mounted" ]]; then
    sudo -n umount -- "$mounted" || return 1
  fi
  rm -rf -- "$scratch"
}
trap cleanup EXIT

# The optional override is for a local container harness providing its own bind.
fixture="${CONFIG_TWO_SAVES_BIND_FIXTURE:-}"
if [[ -z "$fixture" ]]; then
  printf 'port: 18317\n# synthetic single-file bind fixture\n' > "$scratch/backing.yaml"
  touch "$scratch/mounted.yaml"
  chmod 0640 "$scratch/backing.yaml" "$scratch/mounted.yaml"
  sudo -n mount --bind "$scratch/backing.yaml" "$scratch/mounted.yaml"
  mounted="$scratch/mounted.yaml"
  fixture="$mounted"
fi

export CONFIG_TWO_SAVES_BIND_FIXTURE="$fixture"
mountpoint -q -- "$fixture"
test -f "$fixture" && test -w "$fixture"

# A mistyped test name or an unexpected Skip must fail the workflow.
tests=(
  TestSaveConfigPreservesBytesOnFileSizeLimit
  TestOrdinaryConfigCapacityFailurePreservesBothYAMLPaths
  TestConfigInodeRecoverySerializesCommit
  TestConfigHardLinksShareSuccessfulSaves
  TestConfigFileCASWithConcurrentWriters
  TestConfigFileRollbackRestoresCompleteSnapshot
  TestDirectorySaveLinuxHandoff
  TestDirectorySaveLinuxNonreplaceableFile
  TestDirectorySaveLinuxVersionedSnapshot
  TestDirectorySaveLinuxDefaultACL
  TestDirectorySaveLinuxUnopenableParent
  TestDirectorySaveLinuxReadOnlyPreservesSnapshot
  TestConfigSaveCASInodeFailure
  TestConfigSaveCASDirectorySyncFailure
  TestConfigSaveCASPreservesExternalMarker
  TestConfigIntegrityAttributePolicy
  TestReconcileConfigSavePreservesNamedReaderAndDeniesOwningGroup
  TestReconcileConfigSaveDoesNotInheritNewAccessACL
  TestConfigReplacementPreservesMountSemantics
)
pattern="$(IFS='|'; echo "${tests[*]}")"
go test -mod=readonly -race -count=1 -json ./internal/config -run "^($pattern)$" |
  tee "$scratch/go-test.jsonl"

python3 - "$scratch/go-test.jsonl" "${tests[@]}" \
  TestConfigReplacementPreservesMountSemantics/directory \
  TestConfigReplacementPreservesMountSemantics/single_file_bind <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as stream:
    results = {
        event["Test"]
        for event in map(json.loads, stream)
        if event["Action"] == "pass" and "Test" in event
    }

missing = set(sys.argv[2:]) - results
if missing:
    raise SystemExit("Expected Linux config tests did not pass: " + ", ".join(sorted(missing)))
print("Confirmed Linux config test passes:", len(sys.argv[2:]))
PY
