package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderCaddyConfig_Minimal(t *testing.T) {
	rs := Routes{
		{Host: "api.example.com", Backends: []string{"10.0.0.1:8080"}},
	}
	body, err := rs.renderCaddyConfig("unix//tmp/admin.sock")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode rendered: %v", err)
	}
	if !strings.Contains(string(body), `"dial":"10.0.0.1:8080"`) {
		t.Fatalf("missing upstream dial in %s", string(body))
	}
	if !strings.Contains(string(body), `"host":["api.example.com"]`) {
		t.Fatalf("missing host matcher in %s", string(body))
	}
	if strings.Contains(string(body), `"tls"`) {
		t.Fatalf("rendered tls block for an all-auto config (should be omitted): %s", string(body))
	}
}

func TestRenderCaddyConfig_InternalTLS(t *testing.T) {
	rs := Routes{
		{Host: "svc.internal", Backends: []string{"10.0.0.1:8080"}, TLS: "internal"},
	}
	body, _ := rs.renderCaddyConfig("unix//tmp/admin.sock")
	if !strings.Contains(string(body), `"module":"internal"`) {
		t.Fatalf("expected internal issuer policy in %s", string(body))
	}
}

func TestRenderCaddyConfig_HeaderOrderStable(t *testing.T) {
	rs := Routes{{
		Host:     "api.example.com",
		Backends: []string{"10.0.0.1:8080"},
		Headers:  map[string]string{"X-B": "2", "X-A": "1", "X-C": "3"},
	}}
	first, _ := rs.renderCaddyConfig("unix//tmp/admin.sock")
	second, _ := rs.renderCaddyConfig("unix//tmp/admin.sock")
	if string(first) != string(second) {
		t.Fatalf("non-stable header order: would force needless reloads\n%s\nvs\n%s", first, second)
	}
	// And: the rendered headers section sorts X-A before X-B before X-C.
	idxA := strings.Index(string(first), `"X-A"`)
	idxB := strings.Index(string(first), `"X-B"`)
	idxC := strings.Index(string(first), `"X-C"`)
	if idxA < 0 || idxB < 0 || idxC < 0 || !(idxA < idxB && idxB < idxC) {
		t.Fatalf("expected stable sorted header order, got positions A=%d B=%d C=%d", idxA, idxB, idxC)
	}
}

func TestRenderCaddyConfig_RejectsBadTLSMode(t *testing.T) {
	_, err := Routes{{Host: "x", Backends: []string{"y:1"}, TLS: "weird"}}.renderCaddyConfig("unix//tmp/x")
	if err == nil || !strings.Contains(err.Error(), "unknown TLS mode") {
		t.Fatalf("expected unknown TLS mode error, got %v", err)
	}
}

func TestRenderCaddyConfig_RejectsEmptyHost(t *testing.T) {
	_, err := Routes{{Backends: []string{"y:1"}}}.renderCaddyConfig("unix//tmp/x")
	if err == nil {
		t.Fatalf("expected error on missing Host")
	}
}

func TestRenderCaddyConfig_RejectsEmptyBackends(t *testing.T) {
	_, err := Routes{{Host: "x"}}.renderCaddyConfig("unix//tmp/x")
	if err == nil {
		t.Fatalf("expected error on missing Backends")
	}
}

func TestRenderCaddyConfigWith_StorageBlockEmitted(t *testing.T) {
	storage := map[string]any{"module": "etcd3", "endpoints": []string{"http://127.0.0.1:2379"}}
	body, err := Routes{{Host: "x", Backends: []string{"y:1"}}}.renderCaddyConfigWith("unix//tmp/x", storage)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(body), `"storage":{`) || !strings.Contains(string(body), `"module":"etcd3"`) {
		t.Fatalf("expected top-level storage block in %s", string(body))
	}
}

func TestRenderCaddyConfigWith_NilStorageOmits(t *testing.T) {
	body, err := Routes{{Host: "x", Backends: []string{"y:1"}}}.renderCaddyConfigWith("unix//tmp/x", nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(string(body), `"storage"`) {
		t.Fatalf("nil storage should omit the block: %s", string(body))
	}
}

func TestEtcdStorageEndpoints_Parsing(t *testing.T) {
	t.Setenv("WEFT_PROXY_STORAGE_ETCD_ENDPOINTS", "  http://a:2379 , http://b:2379  ,")
	got := EtcdStorageEndpoints()
	if len(got) != 2 || got[0] != "http://a:2379" || got[1] != "http://b:2379" {
		t.Fatalf("parse: got %v", got)
	}
	t.Setenv("WEFT_PROXY_STORAGE_ETCD_ENDPOINTS", "")
	if EtcdStorageEndpoints() != nil {
		t.Fatalf("empty env should return nil slice")
	}
}

func TestRenderCaddyConfig_AutoHTTPSDisableMixed(t *testing.T) {
	rs := Routes{
		{Host: "api.example.com", Backends: []string{"x:1"}},
		{Host: "metrics.example.com", Backends: []string{"y:1"}, TLS: "off"},
	}
	body, _ := rs.renderCaddyConfig("unix//tmp/admin.sock")
	if !strings.Contains(string(body), `"disable":true`) {
		t.Fatalf("expected automatic_https.disable=true when one route opts out: %s", string(body))
	}
}
