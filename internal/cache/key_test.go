package cache

import "testing"

func TestEveryEvidenceDimensionInvalidatesKey(t *testing.T) {
	base := Inputs{
		BaseTree: "base", HeadTree: "head", Worktree: "clean",
		Lockfiles: map[string]string{"go.sum": "lock"}, Toolchains: map[string]string{"go": "1.24"},
		Environment: "env", Contract: "contract", Policy: "policy", Comparator: "comparator", Seed: "seed",
		Budgets: map[string]string{"total": "30m"}, CoreVersion: "core", PackVersions: map[string]string{"go": "pack"},
	}
	original, err := Key(base)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []Inputs{
		clone(base, func(value *Inputs) { value.BaseTree = "changed" }),
		clone(base, func(value *Inputs) { value.HeadTree = "changed" }),
		clone(base, func(value *Inputs) { value.Worktree = "changed" }),
		clone(base, func(value *Inputs) { value.Lockfiles["go.sum"] = "changed" }),
		clone(base, func(value *Inputs) { value.Toolchains["go"] = "changed" }),
		clone(base, func(value *Inputs) { value.Environment = "changed" }),
		clone(base, func(value *Inputs) { value.Contract = "changed" }),
		clone(base, func(value *Inputs) { value.Policy = "changed" }),
		clone(base, func(value *Inputs) { value.Comparator = "changed" }),
		clone(base, func(value *Inputs) { value.Seed = "changed" }),
		clone(base, func(value *Inputs) { value.Budgets["total"] = "changed" }),
		clone(base, func(value *Inputs) { value.CoreVersion = "changed" }),
		clone(base, func(value *Inputs) { value.PackVersions["go"] = "changed" }),
	}
	for index, mutation := range mutations {
		key, err := Key(mutation)
		if err != nil {
			t.Fatal(err)
		}
		if key == original {
			t.Fatalf("mutation %d failed to invalidate cache key", index)
		}
	}
}

func clone(value Inputs, mutate func(*Inputs)) Inputs {
	copy := value
	copy.Lockfiles = map[string]string{}
	for key, item := range value.Lockfiles {
		copy.Lockfiles[key] = item
	}
	copy.Toolchains = map[string]string{}
	for key, item := range value.Toolchains {
		copy.Toolchains[key] = item
	}
	copy.Budgets = map[string]string{}
	for key, item := range value.Budgets {
		copy.Budgets[key] = item
	}
	copy.PackVersions = map[string]string{}
	for key, item := range value.PackVersions {
		copy.PackVersions[key] = item
	}
	mutate(&copy)
	return copy
}
