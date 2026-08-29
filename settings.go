package main

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/widget"
)

// ponytail: fyne Preferences is the store — no config file handling needed
const (
	prefAdapter = "adapter"
	prefPort    = "port"
	prefRate    = "rate"
)

func restoreSettings(p fyne.Preferences, adapterSel *widget.Select, portEntry, rateEntry *widget.SelectEntry) {
	adapterSel.SetSelected("CombiAdapter")
	if a := p.String(prefAdapter); a != "" {
		adapterSel.SetSelected(a) // no-op if the adapter is gone from this build
	}
	portEntry.SetText(p.StringWithFallback(prefPort, "/dev/ttyUSB0"))
	rateEntry.SetText(p.StringWithFallback(prefRate, "500"))
}

func saveSettings(p fyne.Preferences, adapter, port, rate string) {
	p.SetString(prefAdapter, adapter)
	p.SetString(prefPort, port)
	p.SetString(prefRate, rate)
}
