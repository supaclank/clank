package agent

var BuildExtraEnv = buildExtraEnv

// EmitForTest exposes the OpenCode backend's internal emit so the external
// test package can drive emit() concurrently with Stop() in race regression
// tests. It calls the real emit() — no behavior is stubbed.
func (b *OpenCodeBackend) EmitForTest(e Event) { b.emit(e) }
