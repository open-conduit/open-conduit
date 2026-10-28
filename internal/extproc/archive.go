package extproc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// Release archives. A package is published as a .tar.gz holding its one
// package directory (acme/open-conduit-extension.json, acme/acme-extension,
// …), one archive per OS and architecture. InstallSource fetches or reads
// one, checks it and unpacks it into the installer's staging directory, then
// runs Install on the unpacked directory unchanged.

// ErrArchive wraps every refused or unreadable release archive.
var ErrArchive = errors.New("extproc: invalid extension archive")

// ErrPlatform refuses a package whose command was built for another OS or
// architecture than this host's.
var ErrPlatform = errors.New("extproc: the package is built for another platform")

// MaxArchiveSize bounds an archive's unpacked contents.
const MaxArchiveSize = 1 << 30

// Source is where a package comes from: a package directory, or a release
// archive given as a path or an http(s) URL.
type Source struct {
	// Location is a directory, an archive path or an archive URL.
	Location string
	// SHA256 is the archive's expected hex digest ("" for none). It is
	// required for a plain http URL and refused for a directory.
	SHA256 string
	// Client fetches URLs (nil: http.DefaultClient).
	Client *http.Client
}

// IsURL reports whether location names an http(s) URL.
func IsURL(location string) bool {
	u, err := url.Parse(location)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// InstallSource installs or updates a package from a directory or a release
// archive (see Source), then behaves exactly as Install.
func InstallSource(ctx context.Context, root string, src Source, o InstallOptions) (InstallResult, error) {
	want, err := parseDigest(src.SHA256)
	if err != nil {
		return InstallResult{}, err
	}
	if !IsURL(src.Location) {
		if info, err := os.Stat(src.Location); err == nil && info.IsDir() {
			if want != nil {
				return InstallResult{}, fmt.Errorf("%w: --sha256 checks an archive, and %s is a directory", ErrArchive, src.Location)
			}
			return Install(root, src.Location, o)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, stagingDir), 0o755); err != nil {
		return InstallResult{}, err
	}
	tmp, err := os.MkdirTemp(filepath.Join(root, stagingDir), "archive-")
	if err != nil {
		return InstallResult{}, err
	}
	defer func() {
		os.RemoveAll(tmp)
		_ = os.Remove(filepath.Join(root, stagingDir))
	}()
	file := filepath.Join(tmp, "archive.tar.gz")
	if err := fetchArchive(ctx, src, want, file); err != nil {
		return InstallResult{}, err
	}
	dir, err := unpackArchive(file, filepath.Join(tmp, "unpacked"))
	if err != nil {
		return InstallResult{}, err
	}
	return Install(root, dir, o)
}

func parseDigest(s string) ([]byte, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "sha256:")
	if s == "" {
		return nil, nil
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != sha256.Size {
		return nil, fmt.Errorf("%w: --sha256 must be 64 hex digits", ErrArchive)
	}
	return b, nil
}

// fetchArchive copies the archive to dst, checking its digest when want is
// set. A plain http URL without a digest is refused before any request.
func fetchArchive(ctx context.Context, src Source, want []byte, dst string) error {
	var body io.ReadCloser
	if IsURL(src.Location) {
		if strings.HasPrefix(strings.ToLower(src.Location), "http:") && want == nil {
			return fmt.Errorf("%w: %s is plain http; pass --sha256 or use https", ErrArchive, src.Location)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.Location, nil)
		if err != nil {
			return err
		}
		client := src.Client
		if client == nil {
			client = http.DefaultClient
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("%w: fetching %s: %w", ErrArchive, src.Location, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fmt.Errorf("%w: fetching %s: %s", ErrArchive, src.Location, resp.Status)
		}
		body = resp.Body
	} else {
		f, err := os.Open(src.Location)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrArchive, err)
		}
		body = f
	}
	defer body.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(body, MaxArchiveSize+1)); err != nil {
		out.Close()
		return fmt.Errorf("%w: reading %s: %w", ErrArchive, src.Location, err)
	}
	if err := out.Close(); err != nil {
		return err
	}
	if got := h.Sum(nil); want != nil && !bytes.Equal(got, want) {
		return fmt.Errorf("%w: %s has sha256 %x, expected %x", ErrArchive, src.Location, got, want)
	}
	return nil
}

// unpackArchive unpacks a .tar.gz holding one package directory into dst
// and returns that directory. Only directories and regular files are
// accepted; links, devices, absolute paths and paths leaving the package
// directory are refused.
func unpackArchive(file, dst string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("%w: not a gzip archive: %w", ErrArchive, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	top := ""
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrArchive, err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name := hdr.Name
		if strings.Contains(name, `\`) || path.IsAbs(name) {
			return "", fmt.Errorf("%w: %q escapes the package directory", ErrArchive, name)
		}
		clean := path.Clean(strings.TrimPrefix(name, "./"))
		if clean == "." {
			continue
		}
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return "", fmt.Errorf("%w: %q escapes the package directory", ErrArchive, name)
		}
		if strings.HasPrefix(path.Base(clean), "._") {
			continue // AppleDouble metadata macOS tar adds, not package content
		}
		first, _, _ := strings.Cut(clean, "/")
		if top == "" {
			top = first
		} else if first != top {
			return "", fmt.Errorf("%w: it holds %s and %s; it must hold one package directory", ErrArchive, top, first)
		}
		if clean == top && hdr.Typeflag != tar.TypeDir {
			return "", fmt.Errorf("%w: %s is not a directory; the archive must hold one package directory", ErrArchive, top)
		}
		target := filepath.Join(dst, filepath.FromSlash(clean))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
		case tar.TypeReg:
			total += hdr.Size
			if total > MaxArchiveSize {
				return "", fmt.Errorf("%w: it unpacks to more than %d bytes", ErrArchive, MaxArchiveSize)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(hdr.Mode).Perm()&0o755)
			if err != nil {
				return "", fmt.Errorf("%w: %s: %w", ErrArchive, name, err)
			}
			if _, err := io.CopyN(out, tr, hdr.Size); err != nil {
				out.Close()
				return "", fmt.Errorf("%w: %s: %w", ErrArchive, name, err)
			}
			if err := out.Close(); err != nil {
				return "", err
			}
		default:
			return "", fmt.Errorf("%w: %s is a link or special file; a package holds only directories and regular files", ErrArchive, name)
		}
	}
	if top == "" {
		return "", fmt.Errorf("%w: it is empty", ErrArchive)
	}
	return filepath.Join(dst, top), nil
}

// checkPlatform refuses a command binary built for another OS or
// architecture. A command it cannot read as an ELF, Mach-O or PE executable
// (a script, say) passes.
func checkPlatform(bin string) error {
	goos, arches := binaryPlatform(bin)
	if goos == "" {
		return nil
	}
	if goos == runtime.GOOS {
		for _, a := range arches {
			if a == runtime.GOARCH {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: its command %s is built for %s/%s, and this host is %s/%s",
		ErrPlatform, filepath.Base(bin), goos, strings.Join(arches, ","), runtime.GOOS, runtime.GOARCH)
}

// binaryPlatform names the OS and architectures (GOOS and GOARCH spellings)
// an executable was built for, or "" when it is not one this can read.
func binaryPlatform(bin string) (string, []string) {
	if f, err := elf.Open(bin); err == nil {
		defer f.Close()
		goos := "linux"
		switch f.OSABI {
		case elf.ELFOSABI_FREEBSD:
			goos = "freebsd"
		case elf.ELFOSABI_NETBSD:
			goos = "netbsd"
		case elf.ELFOSABI_OPENBSD:
			goos = "openbsd"
		}
		return goos, []string{elfArch(f)}
	}
	if f, err := macho.Open(bin); err == nil {
		defer f.Close()
		return "darwin", []string{machoArch(f.Cpu)}
	}
	if f, err := macho.OpenFat(bin); err == nil {
		defer f.Close()
		var arches []string
		for _, a := range f.Arches {
			arches = append(arches, machoArch(a.Cpu))
		}
		return "darwin", arches
	}
	if f, err := pe.Open(bin); err == nil {
		defer f.Close()
		return "windows", []string{peArch(f.Machine)}
	}
	return "", nil
}

func elfArch(f *elf.File) string {
	switch f.Machine {
	case elf.EM_X86_64:
		return "amd64"
	case elf.EM_AARCH64:
		return "arm64"
	case elf.EM_386:
		return "386"
	case elf.EM_ARM:
		return "arm"
	case elf.EM_RISCV:
		return "riscv64"
	case elf.EM_PPC64:
		if f.ByteOrder.String() == "LittleEndian" {
			return "ppc64le"
		}
		return "ppc64"
	case elf.EM_S390:
		return "s390x"
	case elf.EM_LOONGARCH:
		return "loong64"
	}
	return f.Machine.String()
}

func machoArch(c macho.Cpu) string {
	switch c {
	case macho.CpuAmd64:
		return "amd64"
	case macho.CpuArm64:
		return "arm64"
	case macho.Cpu386:
		return "386"
	case macho.CpuArm:
		return "arm"
	}
	return c.String()
}

func peArch(m uint16) string {
	switch m {
	case pe.IMAGE_FILE_MACHINE_AMD64:
		return "amd64"
	case pe.IMAGE_FILE_MACHINE_ARM64:
		return "arm64"
	case pe.IMAGE_FILE_MACHINE_I386:
		return "386"
	}
	return fmt.Sprintf("machine 0x%x", m)
}
