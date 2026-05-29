# tart-oci

Darwin-only helpers for extracting raw disk images from Tart-compatible OCI
layouts.

## Module

```
github.com/go-diskimages/tart-oci
```

## API

```go
const DiskMediaType = "application/vnd.cirruslabs.tart.disk.v2"

func BlobPath(cacheDir, digest string) string
func ExtractDisk(cacheDir, dst string, w io.Writer) error
```

Depends on `github.com/go-compressions/lzfse` to decompress Tart disk layers.