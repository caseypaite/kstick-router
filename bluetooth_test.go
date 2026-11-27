package main

import "testing"

// Format of `busctl --json=short call org.bluez / org.freedesktop.DBus.ObjectManager GetManagedObjects`
// (adapter part captured from the MF800, 2026-11-26).
const bluezSample = `{"type":"a{oa{sa{sv}}}","data":[{"/org/bluez":{"org.bluez.AgentManager1":{}},
"/org/bluez/hci0":{"org.bluez.Adapter1":{"Address":{"type":"s","data":"02:00:CB:92:58:26"},"Alias":{"type":"s","data":"router1"},"Powered":{"type":"b","data":true},"Discoverable":{"type":"b","data":false},"Pairable":{"type":"b","data":false},"Discovering":{"type":"b","data":false}}},
"/org/bluez/hci0/dev_57_2E_40_48_1E_00":{"org.bluez.Device1":{"Address":{"type":"s","data":"57:2E:40:48:1E:00"},"Paired":{"type":"b","data":false},"Trusted":{"type":"b","data":false},"Connected":{"type":"b","data":false},"RSSI":{"type":"n","data":-80}}},
"/org/bluez/hci0/dev_F8_5B_1B_45_E1_29":{"org.bluez.Device1":{"Address":{"type":"s","data":"F8:5B:1B:45:E1:29"},"Name":{"type":"s","data":"LIVSMT-RO-E128"},"Paired":{"type":"b","data":false},"Trusted":{"type":"b","data":false},"Connected":{"type":"b","data":false},"RSSI":{"type":"n","data":-70}}},
"/org/bluez/hci0/dev_00_1B_66_AA_BB_CC":{"org.bluez.Device1":{"Address":{"type":"s","data":"00:1B:66:AA:BB:CC"},"Name":{"type":"s","data":"Headphones"},"Icon":{"type":"s","data":"audio-headset"},"Paired":{"type":"b","data":true},"Trusted":{"type":"b","data":true},"Connected":{"type":"b","data":true},"UUIDs":{"type":"as","data":["0000110b-0000-1000-8000-00805f9b34fb"]}}}}]}`

func TestParseBlueZ(t *testing.T) {
	ad, devs, err := parseBlueZ([]byte(bluezSample))
	if err != nil {
		t.Fatal(err)
	}
	if ad["address"] != "02:00:CB:92:58:26" || ad["powered"] != true || ad["path"] != "/org/bluez/hci0" {
		t.Errorf("adapter: %v", ad)
	}
	if len(devs) != 3 || devs[0].Name != "Headphones" || !devs[0].Paired || !devs[0].Audio || !devs[0].Connected {
		t.Fatalf("paired first: %+v", devs)
	}
	if devs[1].Name != "LIVSMT-RO-E128" || devs[1].RSSI == nil || *devs[1].RSSI != -70 || devs[2].Name != "" {
		t.Errorf("named before unnamed: %+v %+v", devs[1], devs[2])
	}
	if _, _, err := parseBlueZ([]byte("garbage")); err == nil {
		t.Error("garbage accepted")
	}
	if stripANSI("\x1b[0;93mCHG\x1b[0m Pairing successful") != "CHG Pairing successful" {
		t.Error("ansi")
	}
}
