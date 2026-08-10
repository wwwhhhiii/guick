package main

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
)

func NewModalPopup(message string, canvas fyne.Canvas) *widget.PopUp {
	var modal *widget.PopUp
	closeBtn := widget.NewButton("Close", func() {
		modal.Hide()
	})
	popupContent := container.NewVBox(
		widget.NewLabel(message),
		closeBtn,
	)
	modal = widget.NewModalPopUp(
		popupContent,
		canvas,
	)
	return modal
}

func NewPeerRequestElement(text string, accepted chan<- bool) *fyne.Container {
	return container.NewHBox(
		widget.NewLabel(text),
		widget.NewButton("✔", func() { accepted <- true }),
		widget.NewButton("✖", func() { accepted <- false }),
	)
}

func NewPendingPeerElement(name string, ok chan<- string, cancel chan<- struct{}) *fyne.Container {
	answerEntry := widget.NewEntry()
	answerEntry.SetPlaceHolder("Paste peer answer here")
	okBtn := widget.NewButton("Submit", func() {
		if len(answerEntry.Text) > 0 {
			ok <- answerEntry.Text
		}
	})
	cancelBtn := widget.NewButton("Cancel", func() {
		cancel <- struct{}{}
	})
	return container.NewVBox(
		widget.NewLabel(name),
		answerEntry,
		container.NewHBox(
			okBtn,
			cancelBtn,
		),
	)
}

func CpyPopup(message string, cpy string, canvas fyne.Canvas) *widget.PopUp {
	var modal *widget.PopUp
	closeBtn := widget.NewButton("Close", func() {
		modal.Hide()
	})
	entry := widget.NewEntry()
	entry.SetText(cpy)
	entry.Disable()
	popupContent := container.NewVBox(
		widget.NewLabel(message),
		entry,
		closeBtn,
	)
	modal = widget.NewModalPopUp(
		popupContent,
		canvas,
	)
	return modal
}

func TextPrompt(canvas fyne.Canvas, placeholder string) (*widget.Entry, <-chan string) {
	out := make(chan string)
	entry := widget.NewEntry()
	entry.SetPlaceHolder(placeholder)
	var p *widget.PopUp
	entry.OnSubmitted = func(s string) {
		out <- s
		p.Hide()
	}
	p = widget.NewModalPopUp(entry, canvas)
	p.Resize(fyne.NewSize(100, 20))
	p.Show()
	return entry, out
}
