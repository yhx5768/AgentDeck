package server

import "testing"

func TestParseModelList(t *testing.T) {
	raw := []byte("Available Models:\nauto, GLM-5.3-Flash, DeepSeek-V4-Pro\n")
	ids, err := parseModelList(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || ids[1] != "GLM-5.3-Flash" {
		t.Fatalf("ids = %v", ids)
	}
}
