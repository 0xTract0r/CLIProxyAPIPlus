"""Require execution and success of the named ordinary fork tests."""
import json
import re
import subprocess
import sys
import tempfile
from collections import defaultdict

SDK = "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
API = "github.com/router-for-me/CLIProxyAPI/v8/internal/api"
REFUSAL = "TestOrdinarySelectedCredentialRefusalIsTerminal"
REQUIRED = {
    SDK: {
        REFUSAL,
        "TestOrdinaryClaudeLongContextNeverRewritesRequestedModel",
        "TestOrdinaryClaudeAliasedOneMillionRequiresSubscription",
        *(f"{REFUSAL}/{method}/{status}"
          for method in ("execute", "count", "stream")
          for status in (400, 401, 403, 429, 503)),
    },
    API: {
        "TestOrdinaryCredentialRoutesKeepForeignFilesOutsideMutation",
        "TestOrdinaryCredentialRoutesKeepForeignFilesOutsideMutation//v0/management/auth-files",
        "TestOrdinaryCredentialRoutesKeepForeignFilesOutsideMutation//v8/management/credentials",
        "TestOrdinaryAuthFileListScopeAndLifecycle",
        "TestIndependentOrdinaryCredentialFieldsPersistence",
        "TestUsageImportWriteFailureLeavesCommittedHistory",
        "TestUsageKnownKeyConfigurationCommit",
        "TestServerCodexAPIKeyResponsesStreamingRequestLog",
        "TestRegisteredLegacyClaudeReauthRequiresOneTargetAndAdmin",
    },
    "github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management": {
        "TestOrdinaryLegacyClaudeReauthUsesSelectedFileAndAdmin",
        "TestOrdinaryCodexV0SelectedReauth",
    },
    "github.com/router-for-me/CLIProxyAPI/v8/internal/usage": {"TestKnownUsageKeyTiesFromDisk"},
}


def check(stream):
    actions = defaultdict(list)
    for line in stream:
        event = json.loads(line)
        package = event.get("Package")
        test = event.get("Test", "")
        action = event.get("Action")
        if package in REQUIRED and action in ("start", "run", "pass", "fail", "skip"):
            if not test or test in REQUIRED[package]:
                actions[package, test].append(action)

    failures = []
    for package, tests in REQUIRED.items():
        if actions[package, ""] != ["start", "pass"]:
            failures.append(f"{package} did not execute and pass")
        for test in sorted(tests):
            observed = actions[package, test]
            if observed != ["run", "pass"]:
                failures.append(f"{package} {test}: expected run and pass, got {observed}")
    return failures


def run_selected():
    with tempfile.TemporaryFile(mode="w+", encoding="utf-8") as results:
        for package, names in REQUIRED.items():
            # Select top-level tests in their owning package. check() also
            # requires every declared subtest to report run and pass.
            parents = sorted(name for name in names if "/" not in name)
            pattern = "^(" + "|".join(map(re.escape, parents)) + ")$"
            run = subprocess.run(
                ["go", "test", "-mod=readonly", "-race", "-count=1", "-timeout=120s",
                 "-json", "-run", pattern, package],
                stdout=results,
            )
            if run.returncode:
                print(f"Go tests failed in {package}", file=sys.stderr)
                results.seek(0)
                return check(results) or [f"{package} exited {run.returncode}"]
        results.seek(0)
        return check(results)


if __name__ == "__main__":
    if sys.argv[1:] == ["--run-selected"]:
        missing = run_selected()
    else:
        with open(sys.argv[1], encoding="utf-8") as results:
            missing = check(results)
    if missing:
        print("\n".join(missing), file=sys.stderr)
        sys.exit(1)
    print(f"Verified {sum(map(len, REQUIRED.values()))} required ordinary test results.")
