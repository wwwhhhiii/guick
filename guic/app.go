package main

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/widget"
)

type Application struct {
	app         *fyne.App
	mainWin     *fyne.Window
	chatListWdg *widget.List
}

func NewApplication() *Application {
	a := app.New()
	mainWin := a.NewWindow("Guic")
	mainWin.Resize(fyne.NewSize(800, 600))

	return &Application{
		app:     &a,
		mainWin: &mainWin,
	}
}
