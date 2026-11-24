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
        "TestOrdinarySchedulerSelectionIDContract",
        "TestOrdinarySchedulerRefusalContract",
        "TestOrdinaryExplicitRetryAfterHTTP500KeepsSameCredential",
        *(f"TestOrdinaryExplicitRetryAfterHTTP500KeepsSameCredential/{method}"
          for method in ("execute", "count", "stream")),
        REFUSAL,
        "TestOrdinaryClaudeLongContextNeverRewritesRequestedModel",
        "TestOrdinaryClaudeAliasedOneMillionRequiresSubscription",
        "TestManager_Issue6415_Original401DoesNotReplayAndBlocksRejectedGrant",
        *(f"{REFUSAL}/{method}/{status}"
          for method in ("execute", "count", "stream")
          for status in (400, 401, 403, 429, 503)),
    },
    API: {
        "TestLogSettingsCommitBeforeVisibility",
        *(f"TestLogSettingsCommitBeforeVisibility/{key}" for key in (
            "request-log", "logging-to-file", "logs-max-total-size-mb", "error-logs-max-files",
        )),
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
        "TestRequestRetrySaveFailurePreservesUnrelatedSave",
        "TestLocalAPIKeySaveFailureAndRetry",
        "TestLocalAPIKeysEmptyJSONShape",
        "TestOrdinaryAccountSettingsReenableLegacyRefreshPreference",
        "TestOrdinaryAccountSettingsKeepsOnlyRefreshBlock",
        "TestOrdinaryAccountSettingsKeepsOnlyRefreshBlock/file_reauth_lock_while_manager_is_stale",
        "TestOrdinaryAccountSettingsKeepsOnlyRefreshBlock/runtime_quarantine_on_file",
        "TestOrdinaryAccountSettingsKeepsOnlyRefreshBlock/runtime_sign-in_in_memory",
        "TestOrdinaryAccountSettingsKeepsOnlyRefreshBlock/runtime_sign-in_on_file",
        "TestOrdinaryLegacyClaudeReauthUsesSelectedFileAndAdmin",
        "TestOrdinaryCodexV0SelectedReauth",
        "TestClaudeOAuthV8CallbackPreservesURLSafeState",
        "TestOrdinaryAccountSettingsPreservesOtherFields",
        "TestGetUsageQueuePopsRequestedRecords",
        "TestFetchCredentialQuota_DeclarativeProbeSummaryOnly",
    },
    "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/antigravity/gemini": {
        "TestOrdinaryGeminiAntigravityKeepsExplicitWireAndParallelOrder",
    },
    "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/gemini/claude": {
        "TestOrdinaryGeminiClaudeKeepsUnsignedThinkingAndTool",
    },
    "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/gemini/openai/responses": {
        "TestOrdinaryGeminiResponsesKeepsUnsignedCallAndResult",
    },
    "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers": {
        "TestExecuteStreamWithAuthManager_EmptyClosedStream",
    },
    "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai": {
        "TestResponsesSSEBeforeFirstFramePreservesHTTPRefusal",
        "TestResponsesHandlerSendKeepAliveBeforeFT",
    },
    "github.com/router-for-me/CLIProxyAPI/v8/internal/client/musecode": {
        "TestRelayTransportDropsAndClientCancelDoNotCooldownCredential",
    },
    "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy": {
        "TestOrdinaryRetiredFallbackNotInPublicCatalog",
        "TestOrdinaryServiceHTTPAdmissionCancelsWaitingAndReleasesStream",
        "TestOrdinaryServiceHTTPAdmissionPreservesRetryAfter",
        "TestOrdinaryExternallyImplementedLegacyProviderKeepsExecutableCatalog",
    },
    "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/claude/openai/chat-completions": {
        "TestOrdinaryClaudeBufferedChatTextBoundary",
        "TestOrdinaryClaudeStreamingChatTextStarts",
    },
    "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/claude/openai/responses": {
        "TestOrdinaryClaudeResponsesPreservesInitialText",
        "TestOrdinaryClaudeResponsesIgnoresNonStringInitialText",
    },
    "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor": {
        "TestOrdinaryClaudeExecutePublishesUpstreamTTFT",
        "TestOrdinaryClaudeNearSSEMIMEUsesFirstByte",
        "TestOrdinaryClaudeBufferedChatTextStarts",
        "TestOrdinaryClaudeIndependentPublicMessages",
        "TestOrdinaryClaudeIndependentPublicMessages/json",
        "TestOrdinaryClaudeIndependentPublicMessages/stream",
        "TestOrdinaryClaudePayloadRulesOnPublicMessages",
        "TestOrdinaryClaudePublicTokenCountUsesOnlyCurrentInput",
        "TestOrdinaryGeminiRejectsForeignSignatureBeforeTransport",
        "TestOrdinaryGeminiInteractionsRejectsForeignAndKeepsOpaque",
    },
    "github.com/router-for-me/CLIProxyAPI/v8/test": {
        "TestOrdinarySingleGrantUpstream502AcrossPublicRoutes",
        *(f"TestOrdinarySingleGrantUpstream502AcrossPublicRoutes/{api}/stream={stream}"
          for api in ("claude", "chat", "responses", "gemini")
          for stream in ("false", "true")),
        "TestOrdinarySingleGrantGeminiOutputBoundary",
        "TestOrdinarySingleGrantGeminiOutputBoundary/before_visible_output/responses_empty_sse_eof",
        "TestOrdinarySingleGrantGeminiOutputBoundary/valid_empty_response_completes",
        "TestOrdinarySingleGrantGeminiOutputBoundary/caller_cancels_before_first_frame",
        "TestOrdinarySingleGrantGeminiOutputBoundary/transport_truncated_after_visible_text",
    },
    "github.com/router-for-me/CLIProxyAPI/v8/internal/usage": {"TestKnownUsageKeyTiesFromDisk"},
}

# This watcher uses Linux filesystem notification semantics.
if sys.platform.startswith("linux"):
    watcher = "TestRollupActualWatcherRepeatedSaves"
    REQUIRED["github.com/router-for-me/CLIProxyAPI/v8/internal/watcher"] = {
        watcher, f"{watcher}/directory-default", f"{watcher}/directory-external-replace",
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
    if failures:
        passed = sum(actions[package, test] == ["run", "pass"]
                     for package, tests in REQUIRED.items() for test in tests)
        failures.insert(0, f"Passed {passed}/{sum(map(len, REQUIRED.values()))} required ordinary test results.")
    return failures


def run_selected():
    with tempfile.TemporaryFile(mode="w+", encoding="utf-8") as results:
        exits = []
        for package, names in REQUIRED.items():
            # Select top-level tests in their owning package. check() also
            # requires every declared subtest to report run and pass.
            parents = sorted(name for name in names if "/" not in name)
            pattern = "^(" + "|".join(map(re.escape, parents)) + ")$"
            run = subprocess.run(
                ["go", "test", "-mod=readonly", "-race", "-count=1", "-timeout=120s",
                 "-json", "-run", pattern, package],
                stdout=results, stderr=subprocess.DEVNULL,
            )
            if run.returncode:
                exits.append(f"Go package {package} exited {run.returncode}")
        results.seek(0)
        failures = check(results)
        if exits and not failures:
            failures.append(f"Passed {sum(map(len, REQUIRED.values()))}/{sum(map(len, REQUIRED.values()))} required ordinary test results.")
        return failures + exits


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
