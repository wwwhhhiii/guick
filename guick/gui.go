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

func NewChatTextGrid() *widget.TextGrid {
	textGrid := widget.NewTextGrid()
	textGrid.Scroll = fyne.ScrollBoth
	return textGrid
}
