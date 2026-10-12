package handler

import (
	"testing"

	"ta-payment-back/internal/i18n"
)

func TestLocalizeFieldsOnlyTouchesListedFields(t *testing.T) {
	type change struct {
		Label string `json:"label"`
		After string `json:"after"`
	}
	type row struct {
		Label   string   `json:"label"`
		Name    string   `json:"actor_name"`
		Details []string `json:"details"`
		Changes []change `json:"changes"`
	}
	i18n.SetForTest(map[string]string{"อนุมัติคำขอ TA": "approved a TA request", "ภาคปกติ": "regular program"})
	rows := []row{{Label: "อนุมัติคำขอ TA", Name: "ภาคปกติ", Details: []string{"ภาคปกติ"}, Changes: []change{{Label: "ภาคปกติ", After: "ภาคปกติ"}}}}
	localizeFields("en", &rows, "label", "details")
	r := rows[0]
	if r.Label != "approved a TA request" || r.Details[0] != "regular program" || r.Changes[0].Label != "regular program" {
		t.Fatalf("listed fields not translated: %+v", r)
	}
	if r.Name != "ภาคปกติ" || r.Changes[0].After != "ภาคปกติ" {
		t.Fatalf("data fields must stay as stored: %+v", r)
	}
	localizeFields("th", &rows, "label")
	if rows[0].Label != "approved a TA request" {
		t.Fatal("th must not touch anything")
	}
}

func TestTranslateTreeOnlyListedKeys(t *testing.T) {
	i18n.SetForTest(map[string]string{"ปริญญาตรี": "Bachelor's", "ใหม่": "new"})
	v := map[string]any{
		"level_th":  "ปริญญาตรี",
		"seniority": "ใหม่", // compared by the web app: must stay
		"name":      "ปริญญาตรี",
		"warnings":  []any{"ปริญญาตรี", map[string]any{"label": "มิถุนายน 2569"}},
	}
	if !translateTree(v, false) {
		t.Fatal("expected a change")
	}
	w := v["warnings"].([]any)
	if v["level_th"] != "Bachelor's" || w[0] != "Bachelor's" || w[1].(map[string]any)["label"] != "June 2026" {
		t.Fatalf("listed keys not translated: %+v", v)
	}
	if v["seniority"] != "ใหม่" || v["name"] != "ปริญญาตรี" {
		t.Fatalf("unlisted keys must stay: %+v", v)
	}
}

func TestTranslateTreeMapUnderListedKey(t *testing.T) {
	i18n.SetForTest(map[string]string{})
	v := map[string]any{"month_labels": map[string]any{"2026-06": "มิถุนายน"}, "other": map[string]any{"x": "มิถุนายน"}}
	translateTree(v, false)
	if v["month_labels"].(map[string]any)["2026-06"] != "June" {
		t.Fatalf("map under a listed key: %+v", v)
	}
	if v["other"].(map[string]any)["x"] != "มิถุนายน" {
		t.Fatalf("unlisted map must stay: %+v", v)
	}
}
