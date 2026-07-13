package domain

import "testing"

// The Protocol and Modality enums each maintain two hand-written parallel
// switches — String() and Parse*() — that must stay inverses of each other:
// a constant added to String() but not Parse*() silently breaks the SQL
// VARCHAR round-trip (the value writes fine and reads back as Unknown).
// These tests turn that silent drift into a hard failure: they sweep the
// enum's integer space, and any value String() recognizes must Parse back to
// itself, with no two constants sharing a wire name.

func TestProtocolStringParseRoundTrip(t *testing.T) {
	seen := map[string]Protocol{}

	for i := 1; i <= 64; i++ {
		p := Protocol(i)

		s := p.String()
		if s == unknownLabel {
			continue // not a defined constant
		}

		if prev, dup := seen[s]; dup {
			t.Errorf("wire name %q claimed by two Protocol values: %d and %d", s, prev, p)
		}

		seen[s] = p

		if got := ParseProtocol(s); got != p {
			t.Errorf("ParseProtocol(%q) = %v, want %v — String() knows this constant but ParseProtocol doesn't", s, got, p)
		}
	}

	if len(seen) == 0 {
		t.Fatal("swept 64 values and found no defined Protocol constants — the sweep is broken")
	}
}

func TestModalityStringParseRoundTrip(t *testing.T) {
	seen := map[string]Modality{}

	for i := 0; i <= 64; i++ { // Modality starts at iota 0 (ModalityChat)
		m := Modality(i)

		s := m.String()
		if s == unknownLabel {
			continue
		}

		if prev, dup := seen[s]; dup {
			t.Errorf("wire name %q claimed by two Modality values: %d and %d", s, prev, m)
		}

		seen[s] = m

		got, err := ParseModality(s)
		if err != nil || got != m {
			t.Errorf("ParseModality(%q) = (%v, %v), want %v — String() knows this constant but ParseModality doesn't", s, got, err, m)
		}
	}

	if len(seen) == 0 {
		t.Fatal("swept 0..64 and found no defined Modality constants — the sweep is broken")
	}
}
