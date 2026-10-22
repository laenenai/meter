package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/laenen-ai/meter/internal/domain"
)

func TestDimensionsValidate(t *testing.T) {
	t.Parallel()

	tooMany := domain.Dimensions{}
	for i := range domain.MaxDimensions + 1 {
		tooMany[string(rune('a'+i%26))+strings.Repeat("x", i)] = "v"
	}

	cases := []struct {
		name    string
		dims    domain.Dimensions
		wantErr bool
	}{
		{"nil", nil, false},
		{"empty", domain.Dimensions{}, false},
		{"well-known keys", domain.Dimensions{
			domain.DimensionTier:             "warm",
			domain.DimensionLane:             "interactive",
			domain.DimensionProcessorID:      "ocr",
			domain.DimensionProcessorVersion: "1.4.2",
			domain.DimensionModelID:          "text-embedding-3",
			domain.DimensionRegion:           "eu-west-1",
			domain.DimensionPrincipalID:      "p-1",
			domain.DimensionCapabilityChain:  "root/attenuated",
			domain.DimensionFilesetID:        "fs-1",
			domain.DimensionDocumentID:       "doc-1",
			domain.DimensionAgentRunID:       "run-1",
			domain.DimensionJobID:            "job-1",
		}, false},
		{"consumer-specific key", domain.Dimensions{"kgs.graph_id": "g-1"}, false},
		{"empty key", domain.Dimensions{"": "v"}, true},
		{"key with space", domain.Dimensions{"agent run": "v"}, true},
		{"key with uppercase", domain.Dimensions{"AgentRun": "v"}, true},
		{"key with equals", domain.Dimensions{"a=b": "v"}, true},
		{"key over-long", domain.Dimensions{strings.Repeat("k", domain.MaxDimensionKeyLen+1): "v"}, true},
		{"empty value", domain.Dimensions{"tier": ""}, true},
		{"value with null byte", domain.Dimensions{"tier": "wa\x00rm"}, true},
		{"value with newline", domain.Dimensions{"tier": "wa\nrm"}, true},
		{"value with unit separator", domain.Dimensions{"tier": "wa\x1frm"}, true},
		{"value over-long", domain.Dimensions{"tier": strings.Repeat("v", domain.MaxDimensionValueLen+1)}, true},
		{"too many dimensions", tooMany, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.dims.Validate()
			if tc.wantErr {
				if !errors.Is(err, domain.ErrInvalidEvent) {
					t.Fatalf("Validate error = %v, want ErrInvalidEvent", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

// Canonical is what an adapter groups and compares by — a Postgres
// adapter's compacted counter bucket is keyed on it (CAS §15.3) — so it
// must be order-independent and injective.
func TestDimensionsCanonicalIsStableAndInjective(t *testing.T) {
	t.Parallel()

	a := domain.Dimensions{"tier": "warm", "region": "eu-west-1", "job_id": "j-1"}
	b := domain.Dimensions{"job_id": "j-1", "tier": "warm", "region": "eu-west-1"}
	if a.Canonical() != b.Canonical() {
		t.Fatalf("Canonical depends on map order: %q vs %q", a.Canonical(), b.Canonical())
	}

	if got, want := domain.Dimensions(nil).Canonical(), (domain.Dimensions{}).Canonical(); got != want {
		t.Errorf("nil and empty Canonical differ: %q vs %q", got, want)
	}

	// The classic injectivity trap: concatenating keys and values without
	// an unambiguous separator makes {"ab":"c"} and {"a":"bc"} collide.
	distinct := []domain.Dimensions{
		{"ab": "c"},
		{"a": "bc"},
		{"a": "b", "c": "d"},
		{"a": "b=c"},
		{"tier": "warm"},
		{"tier": "warm", "region": "eu"},
	}
	seen := map[string]int{}
	for i, d := range distinct {
		c := d.Canonical()
		if j, dup := seen[c]; dup {
			t.Fatalf("Canonical collision between %#v and %#v: %q", distinct[j], d, c)
		}
		seen[c] = i
	}
}

func TestDimensionsCloneIsIndependent(t *testing.T) {
	t.Parallel()

	orig := domain.Dimensions{"tier": "warm"}
	clone := orig.Clone()
	clone["tier"] = "cold"
	clone["region"] = "eu"

	if orig["tier"] != "warm" {
		t.Errorf("mutating the clone changed the original: tier = %q", orig["tier"])
	}
	if _, ok := orig["region"]; ok {
		t.Error("adding to the clone changed the original")
	}
	if domain.Dimensions(nil).Clone() != nil {
		t.Error("Clone of nil should stay nil, so an absent dimension set never becomes an empty one")
	}
}

func TestDimensionsEqual(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		a, b domain.Dimensions
		want bool
	}{
		{"nil and empty", nil, domain.Dimensions{}, true},
		{"same content, different order", domain.Dimensions{"a": "1", "b": "2"}, domain.Dimensions{"b": "2", "a": "1"}, true},
		{"different value", domain.Dimensions{"a": "1"}, domain.Dimensions{"a": "2"}, false},
		{"extra key", domain.Dimensions{"a": "1"}, domain.Dimensions{"a": "1", "b": "2"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.a.Equal(tc.b); got != tc.want {
				t.Errorf("Equal = %v, want %v", got, tc.want)
			}
			if got := tc.b.Equal(tc.a); got != tc.want {
				t.Errorf("Equal (reversed) = %v, want %v", got, tc.want)
			}
		})
	}
}
