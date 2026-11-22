package server

import "fmt"

func typeName(v any) string { return fmt.Sprintf("%T", v) }
