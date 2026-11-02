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
	// Permission names the manifest permission the method also needs (""
	// for none): without it the host neither serves nor calls the method.
	Permission string
	// Contract names the public in-process surface the method carries:
	// Go symbols of packages extension, storage and observability, or a
	// host composition hook (prefixed "host:") the daemon drives for
	// in-process extensions today.
	Contract []string
}

// Methods is the protocol version 1 catalogue.
var Methods = []Method{
	// Lifecycle.
	{Name: "initialize", Direction: HostToExtension, Contract: []string{"extension.Registration.Version", "extension.Executor.Capabilities (manifest executors[].capabilities)", "extension.Registration.Name", "extension.Registration.EventTypes"}},
	{Name: "configure", Direction: HostToExtension, Contract: []string{"host:config apply (register<Extension> on OnApply)", "extension.Source", "host:webhook routes"}},
	{Name: "validate", Direction: HostToExtension, Contract: []string{"host:config.Registry validators (connection, extension, route, resource, executor config)", "extension.ResourceModeDefaulter.ModeDefault", "extension.ResourceSandboxChecker.SandboxWorkspace", "extension.ResourceModer.ResourceMode", "inproc.ConfigType.Binding", "extension.RouteBinding"}},
	{Name: "shutdown", Direction: HostToExtension, Contract: []string{"observability.Exporter.Shutdown", "host:daemon stop"}},
	{Name: "$/cancel", Direction: Both, Notification: true, Contract: []string{"context.Context cancellation"}},

	// Ingress: push, reconcile and extension-owned receivers.
	{Name: "ingress.push", Direction: HostToExtension, Provides: "extensions.ingress", Contract: []string{"extension.Ingress.Push", "extension.Ingress.Spec"}},
	{Name: "ingress.reconcile", Direction: HostToExtension, Provides: "extensions.ingress", Contract: []string{"extension.Ingress.Reconcile"}},
	{Name: "webhook.handle", Direction: HostToExtension, Provides: "webhooks", Contract: []string{"host:<extension>.WebhookHandler (verify, then push)"}},
	{Name: "ingress.deliver", Direction: ExtensionToHost, Provides: "extensions.ingress", Contract: []string{"host:push func (runtime Ingress.Push, acknowledged after durable acceptance)"}},

	// Routing and work kinds.
	{Name: "route.match", Direction: HostToExtension, Provides: "routes", Contract: []string{"extension.RouteHandler.Match"}},
	{Name: "route.samples", Direction: HostToExtension, Provides: "routes.samples", Contract: []string{"extension.RouteSampler.Samples"}},
	{Name: "work_kind.trigger", Direction: HostToExtension, Provides: "work_kinds", InTransaction: true, Contract: []string{"host:runtime.WorkKindPolicy.Trigger"}},
	{Name: "recurrence.next", Direction: HostToExtension, Provides: "recurrences", InTransaction: true, Contract: []string{"host:runtime.RecurrenceFunc"}},

	// Toolbelt operations registered by the extension.
	{Name: "operation.call", Direction: HostToExtension, Provides: "operations", Contract: []string{"extension.Operation.Handler", "extension.OperationError", "extension.CallRequestID"}},
	{Name: "operation.wait", Direction: HostToExtension, Provides: "operations.wait", Contract: []string{"extension.Operation.Wait", "extension.WaitPlan", "extension.Dependency"}},
	{Name: "operation.emit", Direction: HostToExtension, Provides: "operations.emit", Contract: []string{"extension.Operation.Emit", "extension.EmitPlan"}},
	{Name: "emit.commit", Direction: HostToExtension, Provides: "operations.commit", InTransaction: true, Contract: []string{"extension.EmitPlan.Commit", "extension.Emitted", "extension.EmitCommit"}},
	{Name: "wait.progress", Direction: ExtensionToHost, Provides: "operations.wait", Contract: []string{"host:runtime.SetWaitProgressInTx", "host:inproc.Host.WaitProgress"}},
	{Name: "dependency.filter", Direction: HostToExtension, Provides: "dependency_filter", InTransaction: true, Contract: []string{"extension.Registration.DependencyFilter"}},
	{Name: "event.render", Direction: HostToExtension, Provides: "render", Contract: []string{"host:renderEvents chain (<extension>.RenderEvent)"}},

	// Executors.
	{Name: "executor.run", Direction: HostToExtension, Provides: "executors", Contract: []string{"extension.Executor.Run", "extension.RunRequest", "extension.ExecutionContext"}},
	{Name: "executor.wait", Direction: HostToExtension, Provides: "executors", Contract: []string{"extension.RunHandle.Wait", "extension.Result", "extension.RetryHintProvider"}},
	{Name: "executor.recover", Direction: HostToExtension, Provides: "executors", Contract: []string{"extension.Executor.Recover"}},
	{Name: "executor.resume", Direction: HostToExtension, Provides: "executors.resume", Contract: []string{"extension.ResumeExecutor.Resume"}},
	{Name: "executor.interrupt", Direction: HostToExtension, Provides: "executors.interrupt", Contract: []string{"extension.InterruptExecutor.Interrupt"}},
	{Name: "executor.inject", Direction: HostToExtension, Provides: "executors.inject", Contract: []string{"extension.InjectExecutor.Inject"}},
	{Name: "executor.stop", Direction: HostToExtension, Provides: "executors.stop", Contract: []string{"extension.StopExecutor.Stop"}},
	{Name: "executor.forget", Direction: HostToExtension, Provides: "executors.forget", Contract: []string{"extension.ForgetExecutor.Forget"}},
	{Name: "executor.attach", Direction: HostToExtension, Provides: "executors.attach", Contract: []string{"extension.AttachExecutor.Attach"}},
	{Name: "execution.persist_recovery", Direction: ExtensionToHost, Provides: "executors", Contract: []string{"extension.RunRequest.PersistRecovery"}},

	// Resources: the broker an executor sees, and providers.
	{Name: "resource.acquire", Direction: ExtensionToHost, Provides: "executors.workspace", Contract: []string{"extension.ResourceBroker.Acquire"}},
	{Name: "resource.lookup", Direction: ExtensionToHost, Provides: "executors.workspace", Contract: []string{"extension.ResourceBroker.Lookup"}},
	{Name: "provider.acquire", Direction: HostToExtension, Provides: "resources", Contract: []string{"extension.ResourceProvider.Acquire"}},
	{Name: "provider.lookup", Direction: HostToExtension, Provides: "resources", Contract: []string{"extension.ResourceProvider.Lookup"}},
	{Name: "provider.reconcile", Direction: HostToExtension, Provides: "resources", Contract: []string{"extension.ResourceProvider.Reconcile"}},
	{Name: "provider.owner_status", Direction: ExtensionToHost, Provides: "resources", Contract: []string{"extension.ResourceStatusFunc"}},

	// Namespaced KV (mandatory, never negotiated) and read-only state.
	{Name: "kv.get", Direction: ExtensionToHost, Contract: []string{"storage.Tx.Get"}},
	{Name: "kv.list", Direction: ExtensionToHost, Contract: []string{"storage.Tx.List"}},
	{Name: "kv.commit", Direction: ExtensionToHost, Contract: []string{"storage.Tx.Put", "storage.Tx.Delete", "storage.Store.Transact"}},
	{Name: "state.work", Direction: ExtensionToHost, Contract: []string{"storage.Tx.Work"}},
	{Name: "state.works", Direction: ExtensionToHost, Contract: []string{"storage.Tx.ListWorks"}},
	{Name: "state.executions", Direction: ExtensionToHost, Contract: []string{"storage.Tx.ListExecutions"}},
	{Name: "state.event", Direction: ExtensionToHost, Contract: []string{"storage.Tx.Event"}},
	{Name: "state.events", Direction: ExtensionToHost, Contract: []string{"storage.Tx.Events"}},
	{Name: "state.scopes", Direction: ExtensionToHost, Contract: []string{"storage.Tx.ListScopes", "storage.Tx.Scope"}},
	{Name: "state.timers", Direction: ExtensionToHost, Contract: []string{"storage.Tx.ListTimers"}},

	// Timers, derived events and control, from background tasks.
	{Name: "timer.arm", Direction: ExtensionToHost, Provides: "recurrences", Contract: []string{"host:runtime.Timers.ArmRecurrence (Armer)"}},
	{Name: "timer.cancel", Direction: ExtensionToHost, Provides: "recurrences", Contract: []string{"host:runtime.Timers.Cancel (Armer)"}},
	{Name: "event.accept", Direction: ExtensionToHost, Provides: "families", Contract: []string{"host:runtime.AcceptDerivedInTx", "host:runtime.AcceptInTx", "host:runtime.Causal.Derive", "extension.Record"}},
	{Name: "work.cancel", Direction: ExtensionToHost, Provides: "work_kinds", Contract: []string{"host:runtime.Control.CancelWork"}},

	// Artifacts and secrets.
	{Name: "artifact.create", Direction: ExtensionToHost, Contract: []string{"storage.Artifacts.Create"}},
	{Name: "artifact.write", Direction: ExtensionToHost, Contract: []string{"storage.ArtifactWriter.Write (io.Writer)"}},
	{Name: "artifact.commit", Direction: ExtensionToHost, Contract: []string{"storage.ArtifactWriter.Commit"}},
	{Name: "artifact.abort", Direction: ExtensionToHost, Contract: []string{"storage.ArtifactWriter.Close (without Commit)"}},
	{Name: "secret.resolve", Direction: ExtensionToHost, Contract: []string{"host:connectionSecrets / resolver.ResolveNames"}},

	// Background tasks, telemetry and operator surfaces.
	{Name: "task.run", Direction: HostToExtension, Provides: "tasks", Contract: []string{"host:Projector.Converge", "host:Ensure", "host:Executor.Maintain", "host:manageHooks", "host:hostWatch", "host:publishJobRuns"}},
	{Name: "telemetry.export", Direction: HostToExtension, Provides: "exporters", Contract: []string{"observability.Exporter.Export"}},
	{Name: "telemetry.sink", Direction: HostToExtension, Permission: PermissionTelemetrySink, Contract: []string{"inproc.Sink.Capture"}},
	{Name: "telemetry.capture", Direction: ExtensionToHost, Permission: PermissionTelemetryCapture, Contract: []string{"inproc.Host.Capture"}},
	{Name: "probe.list", Direction: ExtensionToHost, Permission: PermissionProbes, Contract: []string{"inproc.Host.Probes", "extension.SetupProbe.Name", "extension.SetupProbe.Summary", "extension.SetupProbe.Args"}},
	{Name: "probe.run", Direction: ExtensionToHost, Permission: PermissionProbes, Contract: []string{"extension.SetupProbe.Run", "extension.ProbeResult"}},
	{Name: "config.base", Direction: ExtensionToHost, Permission: PermissionConfigRead, Contract: []string{"inproc.Host.Config", "inproc.ConfigHost.Base", "inproc.ConfigBase"}},
	{Name: "config.edit", Direction: ExtensionToHost, Permission: PermissionConfigRead, Contract: []string{"inproc.ConfigHost.Edit", "inproc.ConfigEdited"}},
	{Name: "config.validate", Direction: ExtensionToHost, Permission: PermissionConfigRead, Contract: []string{"inproc.ConfigHost.Validate", "inproc.ConfigDiagnostic"}},
	{Name: "config.candidate", Direction: HostToExtension, Provides: "config_candidate", Permission: PermissionConfigRead, Contract: []string{"extension.ConfigCandidate.ConfigCandidate", "extension.Candidate", "extension.CandidateChange"}},
	{Name: "config.trace", Direction: ExtensionToHost, Permission: PermissionConfigRead, Contract: []string{"inproc.ConfigHost.Trace", "inproc.CandidateTrace"}},
	{Name: "check.run", Direction: HostToExtension, Provides: "checks", Contract: []string{"host:runtime.Check (Checks, DeepChecks)"}},
	{Name: "run.environment", Direction: HostToExtension, Provides: "run_environment", Contract: []string{"extension.RunEnvironment.RunEnvironment", "extension.RunEnvironmentRequest", "extension.ExecutionContext.Env"}},
	{Name: "work.annotate", Direction: HostToExtension, Provides: "annotate", Contract: []string{"host:management Annotate"}},
	{Name: "work.actions", Direction: HostToExtension, Provides: "actions", Contract: []string{"extension.ActionProvider.Actions", "extension.ActionTarget", "extension.Action"}},
	{Name: "work.act", Direction: HostToExtension, Provides: "actions", Contract: []string{"extension.ActionProvider.Invoke", "extension.ActionCall", "extension.Caller", "extension.ActionPlan", "extension.OperationError"}},
	{Name: "view.render", Direction: HostToExtension, Provides: "panels", Contract: []string{"extension.PanelProvider.Panels", "extension.Panel", "extension.PanelSection"}},

	// Setup runs at CLI time with host prompts and the secret store.
	{Name: "setup.run", Direction: HostToExtension, Provides: "setup", Contract: []string{"extension.Setup.Setup", "extension.Setup.SetupInfo", "extension.SetupResult", "extension.SetupHost.Config", "extension.SetupHost.Cwd", "extension.SetupHost.StateDir", "extension.SetupHost.DryRun"}},
	{Name: "setup.print", Direction: ExtensionToHost, Provides: "setup", Contract: []string{"extension.SetupHost.Printf"}},
	{Name: "setup.prompt", Direction: ExtensionToHost, Provides: "setup", Contract: []string{"extension.SetupHost.Prompt"}},
	{Name: "setup.choose", Direction: ExtensionToHost, Provides: "setup", Contract: []string{"extension.SetupHost.Choose"}},
	{Name: "setup.open_url", Direction: ExtensionToHost, Provides: "setup", Contract: []string{"extension.SetupHost.OpenURL"}},
	{Name: "setup.store_secret", Direction: ExtensionToHost, Provides: "setup", Contract: []string{"extension.SetupHost.StoreSecret"}},
	{Name: "setup.look_path", Direction: ExtensionToHost, Provides: "setup", Contract: []string{"extension.SetupHost.LookPath"}},
	{Name: "setup.run_command", Direction: ExtensionToHost, Provides: "setup", Contract: []string{"extension.SetupHost.Run"}},
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
