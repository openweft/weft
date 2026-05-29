# qcow2

Helpers for inspecting QCOW2 images and converting them to raw disk images.

## Module

```
github.com/go-diskimages/qcow2
```

## API

```go
func IsQCOW2File(path string) bool
func ConvertToRaw(src, dst string, w io.Writer) error
```