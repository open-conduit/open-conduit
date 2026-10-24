# Pinned source test index

Source revision: `c2461e55a9d7ca32527bace9db5ecec8c5e60cb0` (clean working tree, inspected 2026-10-24).

These are source test names, not Open Conduit test results. References in the [parity baseline](parity.md) use this revision. Paths below are relative to `../open-supervisor`.

## `internal/adapter/httpx_test.go`

`TestAPIErrorIsOneLine`

## `internal/adapters/bitbucket/bitbucket_test.go`

`TestDetectRepo`, `TestMarkRoundTrip`, `TestPaginationAndComments`, `TestPRsFor`, `TestChecks`, `TestEnsureWebhook`, `TestParseWebhook`

## `internal/adapters/claudecode/claudecode_test.go`

`TestTurnTakesInAMessageAndEndsWithAStatus`, `TestPromptAndEnv`, `TestParse`, `TestParseUsageLimit`, `TestResetFromText`, `TestTranscript`, `TestPrepareWritesThePlugin`, `TestSetupCommand`, `TestParseCountsCalls`, `TestPerTurnTakesOffTheSessionsEarlierTotals`

## `internal/adapters/github/attach_test.go`

`TestAttachStartsTheBranchThenCommits`, `TestFetchSignsInOnlyOnItsHost`, `TestFetchReadsRepoFilesRaw`

## `internal/adapters/github/github_test.go`

`TestRegistered`, `TestCandidatesPaginatesAndSkipsPRs`, `TestChecksEvaluation`, `TestMarkers`, `TestCommentsFilterSinceAndKinds`, `TestEditLabelsIgnoresMissing`, `TestDetectRepo`, `TestWebhookSignature`, `TestWebhookSignals`, `TestWebhookEvents`, `TestWantEventsSubscribes`, `TestEnsureWebhookPatchesOrCreates`, `TestAppTokens`, `TestOpenIssue`, `TestReactions`, `TestPRsForReadsBranchAndTimeline`, `TestPRsForReadsSharedTrackerTimeline`

## `internal/adapters/github/transport_test.go`

`TestUnchangedPollsAreConditional`, `TestRateLimitPausesUntilReset`, `TestSecondaryRateLimitWaitsForRetryAfter`, `TestForbiddenWithoutRateLimitIsAnOrdinaryError`

## `internal/adapters/jira/jira_test.go`

`TestWikiConversion`, `TestRefs`, `TestCandidatesPaginationAndFallback`, `TestCommentsAndMarks`, `TestLabels`, `TestTransition`, `TestReopenedAt`, `TestParseWebhook`, `TestEnsureWebhookNeedsAdmin`, `TestSeveralProjectsAndRouteAttrs`

## `internal/adapters/linear/linear_test.go`

`TestCandidatesAndAttrs`, `TestBindingAndRefs`, `TestMarks`, `TestParseWebhook`, `TestEditLabelsAndTransition`

## `internal/config/config_test.go`

`TestRoundTrip`, `TestValidate`, `TestLabelsMerge`, `TestSecrets`, `TestLabelsAdmits`, `TestGitFor`, `TestValidateLand`, `TestHost`

## `internal/config/jobs_test.go`

`TestJobValidate`

## `internal/config/sources_test.go`

`TestGlob`, `TestSourcesLoadAndRoundTrip`, `TestSourcesValidate`

## `internal/configcli/configcli_test.go`

`TestSettingsPaths`, `TestBuildASharedSetup`, `TestReviewFixes`

## `internal/core/core_test.go`

`TestRunAtIn`, `TestRender`, `TestEventSame`

## `internal/core/wait_test.go`

`TestParseCond`, `TestHoldIn`, `TestMeets`

## `internal/engine/engine_test.go`

`TestClaimRunAndLabel`, `TestCommentsBecomeTheNextTurnsEvents`, `TestStatusLabelComesOffWhileATurnRuns`, `TestInterjectionReachesTheRunningTurn`, `TestFailedTurnRetriesLater`, `TestUsageLimitHoldsTurnsUntilItLifts`, `TestScheduledIssueWaitsForItsTime`, `TestHoldInAnUnlabelledBodyIsKept`, `TestChecksAndMergeReachTheAgent`, `TestConcurrencyCap`, `TestDotEnv`, `TestColdSessionHandsOffToAFreshOne`, `TestDroppingAnIssueEndsItsTrial`, `TestOptInClaimingAndTheTurnsGitAndAdditions`, `TestSigningKeptWhenAskedFor`

## `internal/engine/host_test.go`

`TestHostWatchAlertsOnCrossingOnly`, `TestHostPropsPutTheFullestDiskFirst`

## `internal/engine/jobs_test.go`

`TestJobMatches`, `TestJobRunsOnEvent`, `TestJobDeliveryWhileRunningRunsAgain`, `TestJobWebhookURL`, `TestCronSlot`, `TestCronSlotClockChanges`, `TestPathsMatch`, `TestJobPathsFilterPushes`, `TestJobTimerRunWaitsForTheOneGoing`, `TestJobRunsAreKept`, `TestJobTimesOut`, `TestJobFreshCheckout`, `TestJobCatchesUpTheMainCheckout`, `TestCronJobRunsAndIsWaitedFor`, `TestDetachedJobIsRecorded`, `TestJobEnvResolvesSecrets`, `TestJobsReloadFromRepoFile`

## `internal/engine/shell_test.go`

`TestShellRunsInItsDirectory`, `TestShellKillReachesWhatItStarted`, `TestShellNeedsADirectoryAndACommand`, `TestShellKeepsPartialLines`

## `internal/engine/source_test.go`

`TestSourceRoutesToProjects`, `TestRoutedIssueStaysWhereItWasClaimed`, `TestUnroutedIssueRoutesOnceItCan`, `TestClaimByHandCanOverrideRoutes`, `TestImplicitSourceKeepsTheOldShape`, `TestHooksAreSharedByTarget`, `TestReloadWithNewRoutesKeepsProjects`, `TestWaitingIssuesAreNeitherClaimedNorUnrouted`, `TestDeliveriesResolveLaterAndRetriesGetThrough`, `TestReloadCarriesAHookToItsNewOwner`

## `internal/engine/triage_test.go`

`TestTriageRunsOnTheProblems`, `TestIsUpstream`, `TestProblemsGoToTelemetryOnce`

## `internal/engine/waits_test.go`

`TestWaitOnAnotherPRWakesTheAgentWhenItMerges`, `TestHeldIssueStartsWhenAnotherAgentReports`, `TestWaitMetWhileNobodyHeardIsReadAtTheReconcile`, `TestAlreadyMetWaitWakesAtOnce`, `TestJobRunEndsAWait`, `TestWaitTimesOutAndCancels`, `TestFinishedIssueDropsItsWait`

## `internal/forms/forms_test.go`

`TestShowIf`, `TestSecretKeyring`, `TestSecretFileFallback`, `TestSecretKeptAndReferences`, `TestIntValidation`, `TestConnectionDraftAndRename`, `TestSuggestions`, `TestProjectDraft`, `TestSettingsDraft`, `TestClone`, `TestProjectNeedsCheckout`

## `internal/forms/sources_test.go`

`TestSharedTrackerOffer`, `TestSharedTrackerDeclined`, `TestSourceDraftRoutes`, `TestSourceDraftKeepsHandWrittenRoutes`, `TestProjectDraftFromSource`

## `internal/host/host_darwin_test.go`

`TestParseLoadAvg`, `TestDecodeUint`, `TestReadsThisMac`

## `internal/host/host_linux_test.go`

`TestParseLoadAvg`, `TestParseMeminfo`

## `internal/host/host_test.go`

`TestShares`, `TestReadMeasuresAMissingPathAtItsParentOnce`

## `internal/playbooks/playbooks_test.go`

`TestSetupGuide`, `TestProjectAdditions`

## `internal/problems/problems_test.go`

`TestWriteAndList`, `TestValidKind`

## `internal/store/store_test.go`

`TestSameSHA`, `TestStore`

## `internal/toolbelt/toolbelt_test.go`

`TestUpstreamIssueURL`, `TestUpstreamIssueIsNeverHeld`, `TestProblemNeedsAKind`, `TestHoldForChecksBeforeAnyCall`

## `internal/tui/editor_test.go`

`TestEditorSourcesTab`, `TestEditorStepsCanAddSteps`

## `internal/tui/jobs_test.go`

`TestRunDetail`

## `internal/tui/shell_test.go`

`TestShellTaskFromThePicker`

## `internal/tui/tui_test.go`

`TestPadKeepsWidth`, `TestWrap`, `TestClean`, `TestClaimSyntax`

## `internal/wizard/wizard_test.go`

`TestSummary`, `TestFoundGit`

## `internal/worktree/kill_unix_test.go`

`TestKillWithinFindsProcessesByWorkingDirectory`

## `internal/worktree/worktree_test.go`

`TestSetupAndRemove`, `TestSetupSkippedWithoutWatchedFiles`, `TestRefresh`, `TestFresh`, `TestSetupFromAnotherRemote`, `TestCatchUp`
