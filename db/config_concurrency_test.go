package db

import "testing"

func TestProfileConcurrencyRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	p := &LLMProfile{Name: "runtime-concurrency", Format: "openai", Model: "synthetic", APIKey: "test-only", MaxConcurrent: 3, MaxTokens: 8192, Retry: RetryOverride{Connect: RetryRule{Attempts: 2}}}
	id, err := d.SaveProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	check := func(want int) {
		t.Helper()
		got, err := d.ProfileByID(id)
		if err != nil || got == nil || got.MaxConcurrent != want || got.MaxTokens != 8192 || got.Retry.Connect.Attempts != 2 {
			t.Fatalf("keyed read: %+v, %v", got, err)
		}
		for _, read := range []func() ([]*LLMProfile, error){d.ListProfiles, d.PoolProfiles} {
			items, err := read()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, item := range items {
				if item.ID == id {
					found = true
					if item.MaxConcurrent != want {
						t.Fatalf("list/pool cap=%d, want %d", item.MaxConcurrent, want)
					}
				}
			}
			if !found {
				t.Fatal("profile missing from read path")
			}
		}
	}
	check(3)
	p.ID, p.APIKey, p.MaxConcurrent = id, "", 5
	if _, err := d.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	check(5)
	got, _ := d.ProfileByID(id)
	if got.APIKey != "test-only" {
		t.Fatal("blank-key update lost stored key")
	}
	p.APIKey, p.MaxConcurrent = "replacement-test-only", 0
	if _, err := d.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	check(0)
	for _, bad := range []int{-1, 1025} {
		p.MaxConcurrent = bad
		if _, err := d.SaveProfile(p); err == nil {
			t.Fatalf("accepted cap %d", bad)
		}
	}
	var defaultCap int
	if err := d.QueryRow(`INSERT INTO llm_profiles(name,format,model) VALUES ('runtime-concurrency-default','openai','synthetic') RETURNING max_concurrent`).Scan(&defaultCap); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM llm_profiles WHERE name='runtime-concurrency-default'`) })
	if defaultCap != 0 {
		t.Fatalf("migration default=%d", defaultCap)
	}
}
