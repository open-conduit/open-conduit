package runtime

import "context"

type actorKey struct{}

// WithActor returns ctx carrying the principal (kind:id, such as
// "operator:alice") a host-served request was authenticated as. Every
// core.manual Event a Control call accepts under ctx records it as its
// Actor. Only the host sets it, from the credential it checked; nothing a
// request carries can.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// ActorFrom is the principal WithActor put on ctx, or "".
func ActorFrom(ctx context.Context) string {
	a, _ := ctx.Value(actorKey{}).(string)
	return a
}
