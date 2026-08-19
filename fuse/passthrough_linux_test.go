package fuse

import "testing"

// TestNegotiatePassthroughIgnoresFlags2OnLegacyInit is a regression test:
// negotiatePassthrough must not trust InitIn.Flags2 for an INIT request
// whose minor version predates it (< 36) — the kernel never wrote those
// bytes for such a request, so they're leftover content from the pooled
// request buffer's previous, unrelated use. Setting the bit that happens to
// land on CAP_PASSTHROUGH's position must not enable passthrough for a
// kernel that never negotiated it.
func TestNegotiatePassthroughIgnoresFlags2OnLegacyInit(t *testing.T) {
	srv := &Server{opts: &MountOptions{EnablePassthrough: true}}
	in := &InitIn{
		Minor:  12, // well below _MINOR_VERSION_INIT_EXT (36)
		Flags2: uint32(CAP_PASSTHROUGH >> 32),
	}
	out := &InitOut{}
	negotiatePassthrough(srv, in, out)
	if out.Flags&CAP_INIT_EXT != 0 || out.Flags2 != 0 {
		t.Fatalf("negotiatePassthrough advertised CAP_PASSTHROUGH for a pre-minor-36 INIT: out=%+v", out)
	}
}

// TestNegotiatePassthroughHonorsFlags2OnModernInit confirms the gate doesn't
// also break the legitimate case: a genuinely modern INIT that carries
// CAP_PASSTHROUGH in Flags2 must still negotiate it.
func TestNegotiatePassthroughHonorsFlags2OnModernInit(t *testing.T) {
	srv := &Server{opts: &MountOptions{EnablePassthrough: true, MaxStackDepth: 2}}
	in := &InitIn{
		Minor:  36,
		Flags2: uint32(CAP_PASSTHROUGH >> 32),
	}
	out := &InitOut{}
	negotiatePassthrough(srv, in, out)
	if out.Flags&CAP_INIT_EXT == 0 || out.Flags2&uint32(CAP_PASSTHROUGH>>32) == 0 {
		t.Fatalf("negotiatePassthrough did not advertise CAP_PASSTHROUGH for a genuine minor-36 INIT: out=%+v", out)
	}
}
