package main

import (
	"encoding/json"
	"testing"
)

func TestModemSummary(t *testing.T) {
	var mo, sim, sig, br map[string]any
	json.Unmarshal([]byte(`{"modem":{"3gpp":{"imei":"111","operator-code":"40410","operator-name":"IND airtel","packet-service-state":"attached","registration-state":"home"},
		"generic":{"access-technologies":["lte"],"current-bands":["utran-1","eutran-3"],"current-modes":"allowed: 4g; preferred: none","own-numbers":[],"power-state":"on",
		"revision":"HIMI_U01_MODEM_V1.0  1  [Sep 09 2015]","hardware-revision":"10000","signal-quality":{"value":"60"},"state":"connected","unlock-retries":["sim-pin (3)","sim-puk (10)"]}}}`), &mo)
	json.Unmarshal([]byte(`{"sim":{"properties":{"imsi":"222","iccid":"333","operator-name":"airtel"}}}`), &sim)
	json.Unmarshal([]byte(`{"modem":{"signal":{"lte":{"rsrp":"-101.00","rsrq":"-11.00","snr":"-0.40","rssi":"-76.00"}}}}`), &sig)
	json.Unmarshal([]byte(`{"bearer":{"status":{"connected":"yes"},"properties":{"apn":"airtelgprs.com"},"ipv4-config":{"address":"100.1.2.3","prefix":"30"},"stats":{"duration":"95"}}}`), &br)
	modemMu.Lock()
	modemCache = map[string]any{"present": true, "modem": mo, "sim": sim, "signal": sig, "bearers": []any{br}}
	modemMu.Unlock()
	defer func() { modemMu.Lock(); modemCache = nil; modemMu.Unlock() }()
	s := modemSummary()
	for k, want := range map[string]string{"imei": "111", "imsi": "222", "iccid": "333", "operator_code": "40410", "rsrp": "-101.00",
		"bands": "utran-1, eutran-3", "firmware": "HIMI_U01_MODEM_V1.0 1 [Sep 09 2015]", "ipv4": "100.1.2.3/30", "apn": "airtelgprs.com",
		"pin_retries": "sim-pin (3), sim-puk (10)", "own_number": "", "packet_service": "attached"} {
		if s[k] != want {
			t.Errorf("%s = %q, want %q", k, s[k], want)
		}
	}
}
