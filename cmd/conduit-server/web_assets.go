package main

import "embed"

//go:generate go run ../../tools/genwasm

//go:embed web/*
var webFS embed.FS
