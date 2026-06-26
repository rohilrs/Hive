package verdict

import "testing"

func TestStageRegistryRegisterLookupRemove(t *testing.T) {
	reg := NewStageRegistry()
	l1 := &Listener{}
	reg.Register("run-1", "implement", l1)
	got, ok := reg.Get("run-1", "implement")
	if !ok || got != l1 {
		t.Errorf("get after register: %p, want %p ok=%v", got, l1, ok)
	}
	reg.Remove("run-1", "implement")
	if _, ok := reg.Get("run-1", "implement"); ok {
		t.Errorf("get after remove should fail")
	}
}

func TestStageRegistryEmptyLookup(t *testing.T) {
	reg := NewStageRegistry()
	if _, ok := reg.Get("nope", "nope"); ok {
		t.Errorf("empty registry shouldn't return anything")
	}
}
