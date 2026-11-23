package auth

import "fmt"

// typeOf names the dynamic type of v for logs (never its message, which may
// carry driver or SQL detail).
func typeOf(v any) string { return fmt.Sprintf("%T", v) }
