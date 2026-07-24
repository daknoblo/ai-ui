package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestParseModelList prüft die Zerlegung getrennter Modell-Listen inkl.
// Trimmen, Leerfeld- und Duplikat-Filterung.
func TestParseModelList(t *testing.T) {
	got := ParseModelList(" gpt-4o, gpt-4o\n gpt-4o-mini \n\n, o3 ,")
	want := []string{"gpt-4o", "gpt-4o-mini", "o3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseModelList = %#v, erwartet %#v", got, want)
	}
	if ParseModelList("   ") != nil {
		t.Fatalf("erwartet nil für leere Eingabe")
	}
}

// TestOverridesApplyAndLocks stellt sicher, dass gesetzte Overrides die
// effektive Konfiguration überlagern und die passenden Locks liefern.
func TestOverridesApplyAndLocks(t *testing.T) {
	o := Overrides{
		Endpoint:            "https://env.example",
		ChatModels:          []string{"gpt-4o"},
		EmbeddingDeployment: "emb-env",
	}
	locks := o.locks()
	if !locks.Endpoint || !locks.ChatModels || !locks.EmbeddingDeployment {
		t.Fatalf("gesetzte Felder müssen gesperrt sein: %+v", locks)
	}
	if locks.ChatDeployment || locks.APIVersion || locks.EmbeddingEndpoint || locks.EmbeddingAPIVersion {
		t.Fatalf("nicht gesetzte Felder dürfen nicht gesperrt sein: %+v", locks)
	}
	if !locks.Any() {
		t.Fatalf("Any() muss true liefern, wenn Felder gesperrt sind")
	}

	base := Config{Endpoint: "https://stored", ChatDeployment: "stored-dep", APIVersion: "v1"}
	eff := o.apply(base)
	if eff.Endpoint != "https://env.example" {
		t.Errorf("Endpoint-Override nicht angewendet: %q", eff.Endpoint)
	}
	if eff.ChatDeployment != "stored-dep" {
		t.Errorf("nicht gesetzter Override darf gespeicherten Wert nicht ändern: %q", eff.ChatDeployment)
	}
	if !reflect.DeepEqual(eff.ChatModels, []string{"gpt-4o"}) {
		t.Errorf("ChatModels-Override nicht angewendet: %#v", eff.ChatModels)
	}
}

// TestStoreGetAppliesOverrides prüft, dass Get() die effektive Konfiguration
// (gespeicherte Werte plus Overrides) liefert.
func TestStoreGetAppliesOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s := NewStore(path, "key", "", "", Overrides{Endpoint: "https://env.example"})
	if _, err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := s.Get().Endpoint; got != "https://env.example" {
		t.Fatalf("Get().Endpoint = %q, erwartet Override", got)
	}
	if !s.Locks().Endpoint {
		t.Fatalf("Endpoint muss gesperrt sein")
	}
}

// TestSaveKeepsLockedFields stellt sicher, dass ein Speichern gesperrte Felder
// weder in der effektiven Konfiguration noch in der Rohdatei verändert, während
// nicht gesperrte Felder normal übernommen werden.
func TestSaveKeepsLockedFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s := NewStore(path, "key", "", "", Overrides{Endpoint: "https://env.example"})
	if _, err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Versuch, das gesperrte Endpoint-Feld zu überschreiben und zugleich ein
	// nicht gesperrtes Feld zu setzen.
	cfg := s.Get()
	cfg.Endpoint = "https://versuch-ueberschreiben"
	cfg.ChatDeployment = "gpt-4o"
	if err := s.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if got := s.Get().Endpoint; got != "https://env.example" {
		t.Errorf("gesperrtes Endpoint wurde verändert: %q", got)
	}
	if got := s.Get().ChatDeployment; got != "gpt-4o" {
		t.Errorf("nicht gesperrtes Feld wurde nicht gespeichert: %q", got)
	}

	// Rohdatei darf den ENV-Wert nicht enthalten.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw Config
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if raw.Endpoint != "" {
		t.Errorf("Rohkonfiguration darf ENV-/UI-Wert nicht speichern, war %q", raw.Endpoint)
	}
	if raw.ChatDeployment != "gpt-4o" {
		t.Errorf("nicht gesperrtes Feld fehlt in Rohkonfiguration: %q", raw.ChatDeployment)
	}
}

// TestSetChatModelUsesEffectiveList erlaubt die Auswahl eines Modells, das nur
// über den ENV-Override der Modell-Liste bereitgestellt wird.
func TestSetChatModelUsesEffectiveList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s := NewStore(path, "key", "", "", Overrides{ChatModels: []string{"gpt-4o", "o3"}})
	if _, err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.SetChatModel("o3"); err != nil {
		t.Fatalf("SetChatModel(o3) sollte erlaubt sein: %v", err)
	}
	if got := s.Get().ChatModel; got != "o3" {
		t.Errorf("ChatModel = %q, erwartet o3", got)
	}
	if err := s.SetChatModel("unbekannt"); err == nil {
		t.Errorf("unbekanntes Modell muss abgelehnt werden")
	}
}

// TestIsConfiguredWithOverrides bestätigt, dass per ENV gesetzte Endpoint-Werte
// zur Konfiguriert-Erkennung beitragen.
func TestIsConfiguredWithOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s := NewStore(path, "key", "", "", Overrides{
		Endpoint:       "https://env.example",
		ChatDeployment: "gpt-4o",
	})
	if _, err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// APIVersion stammt aus den Defaults, Endpoint/Deployment aus ENV, Key gesetzt.
	if !s.IsConfigured() {
		t.Fatalf("erwartet konfiguriert mit ENV-Overrides")
	}

	// Ohne API-Key darf nicht als konfiguriert gelten.
	s2 := NewStore(filepath.Join(t.TempDir(), "config.json"), "", "", "", Overrides{
		Endpoint:       "https://env.example",
		ChatDeployment: "gpt-4o",
	})
	if _, err := s2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s2.IsConfigured() {
		t.Fatalf("ohne API-Key darf nicht konfiguriert sein")
	}
}
