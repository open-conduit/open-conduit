package protocol

// Direction says which side sends a method's requests.
type Direction string

const (
	// HostToExtension methods are served by the extension process.
	HostToExtension Direction = "host_to_extension"
	// ExtensionToHost methods are served by the host and scoped by it to
	// the extension's manifest name, configured scopes and authority.
	ExtensionToHost Direction = "extension_to_host"
	// Both is the cancellation notification, which either side may send.
	Both Direction = "both"
)

// Method is one wire method of protocol version 1.
type Method struct {
	Name      string
	Direction Direction
	// Notification methods carry no id and get no response.
	Notification bool
	// InTransaction methods are called by the host while it holds a
	// coordination transaction. They must be pure and deterministic, are
	// bounded by InTransactionDeadlineMillis, and may make no callback
	// (the host refuses one with CodeReentrant). A timeout or transport
	// failure aborts that transaction with a retryable error.
	InTransaction bool
	// Provides names the manifest entry that enables the method ("" for
	// lifecycle methods every extension serves or may call).
	Provides string
	// Contract names the public in-process surface the method carries:
	// Go symbols of packages extension, storage and observability, or a
	// host composition hook (prefixed "host:") the daemon drives for
	// in-process extensions today.
	Contract []string
	// UseCases names the shipped first-party extensions and executors that
	// need the interaction (07-01 inventory). Every method has at least one.
	UseCases []string
}

// Methods is the protocol version 1 catalogue.
var Methods = []Method{
	// Lifecycle.
	{Name: "initialize", Direction: HostToExtension, Contract: []string{"extension.Registration.Version", "extension.Executor.Capabilities (manifest executors[].capabilities)", "extension.Registration.Name", "extension.Registration.EventTypes"}, UseCases: []string{"github", "slack", "linear", "jira", "bitbucket", "schedule", "introspection", "posthog", "git", "shell", "claude-code"}},
	{Name: "configure", Direction: HostToExtension, Contract: []string{"host:config apply (register<Extension> on OnApply)", "extension.Source", "host:webhook routes"}, UseCases: []string{"github", "slack", "linear", "jira", "bitbucket", "schedule", "introspection", "posthog"}},
	{Name: "validate", Direction: HostToExtension, Contract: []string{"host:config.Registry validators (connection, extension, route, resource, executor config)"}, UseCases: []string{"github", "slack", "linear", "jira", "bitbucket", "schedule", "introspection", "posthog", "git", "shell", "claude-code", "legacy-job"}},
	{Name: "shutdown", Direction: HostToExtension, Contract: []string{"observability.Exporter.Shutdown", "host:daemon stop"}, UseCases: []string{"posthog", "introspection", "slack"}},
	{Name: "$/cancel", Direction: Both, Notification: true, Contract: []string{"context.Context cancellation"}, UseCases: []string{"github", "slack", "claude-code", "shell"}},

	// Ingress: push, reconcile and extension-owned receivers.
	{Name: "ingress.push", Direction: HostToExtension, Provides: "extensions.ingress", Contract: []string{"extension.Ingress.Push", "extension.Ingress.Spec"}, UseCases: []string{"github", "slack", "linear", "jira", "bitbucket", "schedule"}},
	{Name: "ingress.reconcile", Direction: HostToExtension, Provides: "extensions.ingress", Contract: []string{"extension.Ingress.Reconcile"}, UseCases: []string{"github", "slack", "linear", "jira", "bitbucket", "schedule"}},
	{Name: "webhook.handle", Direction: HostToExtension, Provides: "webhooks", Contract: []string{"host:<extension>.WebhookHandler (verify, then push)"}, UseCases: []string{"github", "slack", "linear", "jira", "bitbucket", "schedule"}},
	{Name: "ingress.deliver", Direction: ExtensionToHost, Provides: "extensions.ingress", Contract: []string{"host:push func (runtime Ingress.Push, acknowledged after durable acceptance)"}, UseCases: []string{"slack"}},

	// Routing and work kinds.
	{Name: "route.match", Direction: HostToExtension, Provides: "routes", Contract: []string{"extension.RouteHandler.Match"}, UseCases: []string{"github", "slack", "linear", "jira", "bitbucket", "schedule", "introspection"}},
	{Name: "route.samples", Direction: HostToExtension, Provides: "routes.samples", Contract: []string{"extension.RouteSampler.Samples"}, UseCases: []string{"slack"}},
	{Name: "work_kind.trigger", Direction: HostToExtension, Provides: "work_kinds", InTransaction: true, Contract: []string{"host:runtime.WorkKindPolicy.Trigger"}, UseCases: []string{"schedule"}},
	{Name: "recurrence.next", Direction: HostToExtension, Provides: "recurrences", InTransaction: true, Contract: []string{"host:runtime.RecurrenceFunc"}, UseCases: []string{"schedule", "introspection"}},

	// Toolbelt operations registered by the extension.
	{Name: "operation.call", Direction: HostToExtension, Provides: "operations", Contract: []string{"extension.Operation.Handler", "extension.OperationError", "extension.CallRequestID"}, UseCases: []string{"github", "slack", "linear", "jira", "bitbucket", "introspection"}},
	{Name: "operation.wait", Direction: HostToExtension, Provides: "operations.wait", Contract: []string{"extension.Operation.Wait", "extension.WaitPlan", "extension.Dependency"}, UseCases: []string{"github"}},
	{Name: "operation.emit", Direction: HostToExtension, Provides: "operations.emit", Contract: []string{"extension.Operation.Emit", "extension.EmitPlan"}, UseCases: []string{"introspection"}},
	{Name: "emit.commit", Direction: HostToExtension, Provides: "operations.commit", InTransaction: true, Contract: []string{"extension.EmitPlan.Commit", "extension.Emitted", "extension.EmitCommit"}, UseCases: []string{"introspection"}},
	{Name: "wait.progress", Direction: ExtensionToHost, Provides: "operations.wait", Contract: []string{"host:runtime.SetWaitProgressInTx", "host:inproc.Host.WaitProgress"}, UseCases: []string{"github"}},
	{Name: "dependency.filter", Direction: HostToExtension, Provides: "dependency_filter", InTransaction: true, Contract: []string{"extension.Registration.DependencyFilter"}, UseCases: []string{"github"}},
	{Name: "event.render", Direction: HostToExtension, Provides: "render", Contract: []string{"host:renderEvents chain (<extension>.RenderEvent)"}, UseCases: []string{"github", "slack", "introspection"}},

	// Executors.
	{Name: "executor.run", Direction: HostToExtension, Provides: "executors", Contract: []string{"extension.Executor.Run", "extension.RunRequest", "extension.ExecutionContext"}, UseCases: []string{"shell", "claude-code", "legacy-job", "github"}},
	{Name: "executor.wait", Direction: HostToExtension, Provides: "executors", Contract: []string{"extension.RunHandle.Wait", "extension.Result", "extension.RetryHintProvider"}, UseCases: []string{"shell", "claude-code", "legacy-job", "github"}},
	{Name: "executor.recover", Direction: HostToExtension, Provides: "executors", Contract: []string{"extension.Executor.Recover"}, UseCases: []string{"shell", "claude-code", "legacy-job"}},
	{Name: "executor.resume", Direction: HostToExtension, Provides: "executors.resume", Contract: []string{"extension.ResumeExecutor.Resume"}, UseCases: []string{"shell", "claude-code", "legacy-job"}},
	{Name: "executor.interrupt", Direction: HostToExtension, Provides: "executors.interrupt", Contract: []string{"extension.InterruptExecutor.Interrupt"}, UseCases: []string{"shell", "claude-code", "legacy-job"}},
	{Name: "executor.inject", Direction: HostToExtension, Provides: "executors.inject", Contract: []string{"extension.InjectExecutor.Inject"}, UseCases: []string{"shell", "claude-code", "legacy-job"}},
	{Name: "executor.stop", Direction: HostToExtension, Provides: "executors.stop", Contract: []string{"extension.StopExecutor.Stop"}, UseCases: []string{"shell", "claude-code", "legacy-job"}},
	{Name: "executor.forget", Direction: HostToExtension, Provides: "executors.forget", Contract: []string{"extension.ForgetExecutor.Forget"}, UseCases: []string{"shell", "claude-code", "legacy-job"}},
	{Name: "executor.attach", Direction: HostToExtension, Provides: "executors.attach", Contract: []string{"extension.AttachExecutor.Attach"}, UseCases: []string{"claude-code"}},
	{Name: "execution.persist_recovery", Direction: ExtensionToHost, Provides: "executors", Contract: []string{"extension.RunRequest.PersistRecovery"}, UseCases: []string{"shell", "claude-code", "legacy-job", "github"}},

	// Resources: the broker an executor sees, and providers.
	{Name: "resource.acquire", Direction: ExtensionToHost, Provides: "executors.workspace", Contract: []string{"extension.ResourceBroker.Acquire"}, UseCases: []string{"claude-code", "legacy-job"}},
	{Name: "resource.lookup", Direction: ExtensionToHost, Provides: "executors.workspace", Contract: []string{"extension.ResourceBroker.Lookup"}, UseCases: []string{"claude-code", "legacy-job"}},
	{Name: "provider.acquire", Direction: HostToExtension, Provides: "resources", Contract: []string{"extension.ResourceProvider.Acquire"}, UseCases: []string{"git"}},
	{Name: "provider.lookup", Direction: HostToExtension, Provides: "resources", Contract: []string{"extension.ResourceProvider.Lookup"}, UseCases: []string{"git"}},
	{Name: "provider.reconcile", Direction: HostToExtension, Provides: "resources", Contract: []string{"extension.ResourceProvider.Reconcile"}, UseCases: []string{"git"}},
	{Name: "provider.owner_status", Direction: ExtensionToHost, Provides: "resources", Contract: []string{"extension.ResourceStatusFunc"}, UseCases: []string{"git"}},

	// Namespaced KV (mandatory, never negotiated) and read-only state.
	{Name: "kv.get", Direction: ExtensionToHost, Contract: []string{"storage.Tx.Get"}, UseCases: []string{"github", "slack", "linear", "jira", "bitbucket", "schedule", "introspection", "claude-code"}},
	{Name: "kv.list", Direction: ExtensionToHost, Contract: []string{"storage.Tx.List"}, UseCases: []string{"github", "slack", "linear", "jira", "introspection", "claude-code"}},
	{Name: "kv.commit", Direction: ExtensionToHost, Contract: []string{"storage.Tx.Put", "storage.Tx.Delete", "storage.Store.Transact"}, UseCases: []string{"github", "slack", "linear", "jira", "bitbucket", "schedule", "introspection", "claude-code"}},
	{Name: "state.work", Direction: ExtensionToHost, Contract: []string{"storage.Tx.Work"}, UseCases: []string{"github", "slack", "linear", "jira", "bitbucket", "claude-code"}},
	{Name: "state.works", Direction: ExtensionToHost, Contract: []string{"storage.Tx.ListWorks"}, UseCases: []string{"github", "slack", "linear", "jira", "schedule", "introspection"}},
	{Name: "state.executions", Direction: ExtensionToHost, Contract: []string{"storage.Tx.ListExecutions"}, UseCases: []string{"github", "slack", "linear", "jira", "schedule"}},
	{Name: "state.event", Direction: ExtensionToHost, Contract: []string{"storage.Tx.Event"}, UseCases: []string{"github"}},
	{Name: "state.events", Direction: ExtensionToHost, Contract: []string{"storage.Tx.Events"}, UseCases: []string{"github", "schedule"}},
	{Name: "state.scopes", Direction: ExtensionToHost, Contract: []string{"storage.Tx.ListScopes", "storage.Tx.Scope"}, UseCases: []string{"github", "introspection"}},
	{Name: "state.timers", Direction: ExtensionToHost, Contract: []string{"storage.Tx.ListTimers"}, UseCases: []string{"schedule", "introspection"}},

	// Timers, derived events and control, from background tasks.
	{Name: "timer.arm", Direction: ExtensionToHost, Provides: "recurrences", Contract: []string{"host:runtime.Timers.ArmRecurrence (Armer)"}, UseCases: []string{"schedule", "introspection"}},
	{Name: "timer.cancel", Direction: ExtensionToHost, Provides: "recurrences", Contract: []string{"host:runtime.Timers.Cancel (Armer)"}, UseCases: []string{"schedule", "introspection"}},
	{Name: "event.accept", Direction: ExtensionToHost, Provides: "families", Contract: []string{"host:runtime.AcceptDerivedInTx", "host:runtime.AcceptInTx", "host:runtime.Causal.Derive", "extension.Record"}, UseCases: []string{"github", "schedule", "introspection"}},
	{Name: "work.cancel", Direction: ExtensionToHost, Provides: "work_kinds", Contract: []string{"host:runtime.Control.CancelWork"}, UseCases: []string{"introspection"}},

	// Artifacts and secrets.
	{Name: "artifact.create", Direction: ExtensionToHost, Contract: []string{"storage.Artifacts.Create"}, UseCases: []string{"shell", "claude-code", "legacy-job"}},
	{Name: "artifact.write", Direction: ExtensionToHost, Contract: []string{"storage.ArtifactWriter.Write (io.Writer)"}, UseCases: []string{"shell", "claude-code", "legacy-job"}},
	{Name: "artifact.commit", Direction: ExtensionToHost, Contract: []string{"storage.ArtifactWriter.Commit"}, UseCases: []string{"shell", "claude-code", "legacy-job"}},
	{Name: "artifact.abort", Direction: ExtensionToHost, Contract: []string{"storage.ArtifactWriter.Close (without Commit)"}, UseCases: []string{"shell", "claude-code", "legacy-job"}},
	{Name: "secret.resolve", Direction: ExtensionToHost, Contract: []string{"host:connectionSecrets / resolver.ResolveNames"}, UseCases: []string{"github", "slack", "linear", "jira", "bitbucket", "schedule", "posthog"}},

	// Background tasks, telemetry and operator surfaces.
	{Name: "task.run", Direction: HostToExtension, Provides: "tasks", Contract: []string{"host:Projector.Converge", "host:Ensure", "host:Executor.Maintain", "host:manageHooks", "host:hostWatch", "host:publishJobRuns"}, UseCases: []string{"github", "slack", "linear", "jira", "bitbucket", "schedule", "introspection", "claude-code"}},
	{Name: "telemetry.export", Direction: HostToExtension, Provides: "exporters", Contract: []string{"observability.Exporter.Export"}, UseCases: []string{"posthog", "introspection"}},
	{Name: "check.run", Direction: HostToExtension, Provides: "checks", Contract: []string{"host:runtime.Check (Checks, DeepChecks)"}, UseCases: []string{"github", "slack", "linear", "jira", "bitbucket", "schedule", "introspection", "posthog", "git", "claude-code"}},
	{Name: "run.environment", Direction: HostToExtension, Provides: "run_environment", Contract: []string{"extension.RunEnvironment.RunEnvironment", "extension.RunEnvironmentRequest", "extension.ExecutionContext.Env"}, UseCases: []string{"github"}},
	{Name: "work.annotate", Direction: HostToExtension, Provides: "annotate", Contract: []string{"host:management Annotate"}, UseCases: []string{"github"}},
	{Name: "work.actions", Direction: HostToExtension, Provides: "actions", Contract: []string{"extension.ActionProvider.Actions", "extension.ActionTarget", "extension.Action"}, UseCases: []string{"github"}},
	{Name: "work.act", Direction: HostToExtension, Provides: "actions", Contract: []string{"extension.ActionProvider.Invoke", "extension.ActionCall", "extension.Caller", "extension.ActionPlan", "extension.OperationError"}, UseCases: []string{"github"}},
	{Name: "view.render", Direction: HostToExtension, Provides: "panels", Contract: []string{"extension.PanelProvider.Panels", "extension.Panel", "extension.PanelSection"}, UseCases: []string{"human"}},

	// Setup runs at CLI time with host prompts and the secret store.
	{Name: "setup.run", Direction: HostToExtension, Provides: "setup", Contract: []string{"extension.Setup.Setup", "extension.Setup.SetupInfo", "extension.SetupResult", "extension.SetupHost.Config", "extension.SetupHost.Cwd", "extension.SetupHost.StateDir", "extension.SetupHost.DryRun"}, UseCases: []string{"github", "slack", "linear", "jira"}},
	{Name: "setup.print", Direction: ExtensionToHost, Provides: "setup", Contract: []string{"extension.SetupHost.Printf"}, UseCases: []string{"slack", "linear", "jira"}},
	{Name: "setup.prompt", Direction: ExtensionToHost, Provides: "setup", Contract: []string{"extension.SetupHost.Prompt"}, UseCases: []string{"slack", "linear", "jira"}},
	{Name: "setup.choose", Direction: ExtensionToHost, Provides: "setup", Contract: []string{"extension.SetupHost.Choose"}, UseCases: []string{"slack"}},
	{Name: "setup.open_url", Direction: ExtensionToHost, Provides: "setup", Contract: []string{"extension.SetupHost.OpenURL"}, UseCases: []string{"slack", "linear", "jira"}},
	{Name: "setup.store_secret", Direction: ExtensionToHost, Provides: "setup", Contract: []string{"extension.SetupHost.StoreSecret"}, UseCases: []string{"slack", "linear", "jira"}},
	{Name: "setup.look_path", Direction: ExtensionToHost, Provides: "setup", Contract: []string{"extension.SetupHost.LookPath"}, UseCases: []string{"slack"}},
	{Name: "setup.run_command", Direction: ExtensionToHost, Provides: "setup", Contract: []string{"extension.SetupHost.Run"}, UseCases: []string{"slack"}},
}

// Lookup returns the catalogue entry for name.
func Lookup(name string) (Method, bool) {
	for _, m := range Methods {
		if m.Name == name {
			return m, true
		}
	}
	return Method{}, false
}
