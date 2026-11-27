package main

import "testing"

// Real output from the MF800 (Airtel, band 40), 2026-11-27.
const mmcliCellInfo = `modem.generic.cell-info.length   : 3
modem.generic.cell-info.value[1] : cell type: lte, serving: no, operator id: 40410, tac: 87D, physical ci: 1E, earfcn: 39151, rsrp: -92.100000, rsrq: -14.400000
modem.generic.cell-info.value[2] : cell type: lte, serving: no, operator id: 40410, tac: 87D, physical ci: 20, earfcn: 39151, rsrp: -93.200000, rsrq: -15.500000
modem.generic.cell-info.value[3] : cell type: lte, serving: yes, operator id: 40410, tac: 87D, ci: E7F220C, physical ci: 1F, earfcn: 39151, rsrp: -87.000000, rsrq: -10.400000, timing advance: 20
`

func TestParseCellInfo(t *testing.T) {
	cells := parseCellInfo(mmcliCellInfo)
	if len(cells) != 3 {
		t.Fatalf("%d cells", len(cells))
	}
	s := cells[2]
	if !s.Serving || s.OperatorID != "40410" || s.TAC != 2173 || s.CI != 243212812 || s.PCI != 31 || s.EARFCN != 39151 || s.TimingAdvanceU != 20 || s.RSRP != -87 {
		t.Errorf("serving cell: %+v", s)
	}
	if n := cells[0]; n.Serving || n.CI != -1 || n.PCI != 30 || n.TimingAdvanceU != -1 {
		t.Errorf("neighbour: %+v", n)
	}
	if s.CI>>8 != 950050 || s.CI&0xFF != 12 {
		t.Errorf("eNB %d-%d", s.CI>>8, s.CI&0xFF)
	}
	if b, f := lteBand(39151); b != 40 || f < 2350 || f > 2350.2 {
		t.Errorf("EARFCN 39151: band %d, %.1f MHz", b, f)
	}
	if b, _ := lteBand(1650); b != 3 {
		t.Errorf("EARFCN 1650: band %d", b)
	}
	if b, _ := lteBand(99999); b != 0 {
		t.Errorf("unknown EARFCN: band %d", b)
	}
	if d := taDistanceM(20); d != 3000 {
		t.Errorf("TA 20 µs: %d m", d)
	}
	for m, want := range map[int]string{450: "450m", 3000: "3.0km", 12750: "13km"} {
		if got := fmtDist(m); got != want {
			t.Errorf("fmtDist(%d) = %q, want %q", m, got, want)
		}
	}
}

func TestOpenCellIDKeyFormat(t *testing.T) {
	for k, want := range map[string]bool{"pk.0123456789abcdef0123456789abcdef": true, "abc_DEF-123.x": true,
		"short": false, "has space inside": false, "pk.abc;rm -rf": false, "": false} {
		if got := reOpenCellIDKey.MatchString(k); got != want {
			t.Errorf("%q: %v, want %v", k, got, want)
		}
	}
}
