package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/mattn/go-isatty"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	tarball "github.com/google/go-containerregistry/pkg/v1/tarball"
)

var (
	tag   = flag.String("tag", "", `Tag to apply to the image (default "docker-squash-$TIMESTAMP_UNIX_NANOS")`)
	quiet = flag.Bool("quiet", false, "Don't show progress")
)

func printBasicUsage() {
	fmt.Fprintf(os.Stderr, "Usage: %s [ OPTIONS ... ] SOURCE DEST\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "Try '%s --help' for more information.\n", os.Args[0])
}

func printHelp() {
	fmt.Fprintf(os.Stdout, `
Usage: %s [ OPTIONS ...] SOURCE DEST

SOURCE can be either:
- A local tarball archive path, like "/path/to/image.tar"
- A remote image ref prefixed with "docker://", like "docker://example:foo"

DEST is the output tarball archive path.

Options:
`, os.Args[0])
	flag.CommandLine.SetOutput(os.Stdout)
	flag.PrintDefaults()
}

func main() {
	flag.CommandLine.Init(os.Args[0], flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	if err := flag.CommandLine.Parse(os.Args[1:]); err != nil {
		if err == flag.ErrHelp {
			printHelp()
			return
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		printBasicUsage()
		os.Exit(1)
	}

	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(1)
	}

	infile := flag.Arg(0)
	outfile := flag.Arg(1)
	outRef, err := name.ParseReference(*tag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if *tag == "" {
		*tag = "docker-squash-" + fmt.Sprintf("%d", time.Now().UnixNano())
	}

	if err := run(infile, outfile, outRef); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func logf(format string, args ...any) {
	if *quiet {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func run(inputPath, outputPath string, outRef name.Reference) error {
	var img v1.Image
	var err error
	if strings.HasPrefix(inputPath, "docker://") {
		ref, err := name.ParseReference(strings.TrimPrefix(inputPath, "docker://"))
		if err != nil {
			return fmt.Errorf("parse input reference: %w", err)
		}
		img, err = remote.Image(ref, remote.WithAuthFromKeychain(authn.DefaultKeychain))
		if err != nil {
			return fmt.Errorf("pull image %q: %w", ref, err)
		}
	} else {
		img, err = tarball.ImageFromPath(inputPath, nil)
		if err != nil {
			return fmt.Errorf("read image tarball from %q: %w", inputPath, err)
		}
	}

	// TODO: handle multi-arch images
	// For now assume single-arch.

	f, err := os.CreateTemp("", "docker-squash-*.tar")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	// Make sure we clean up the temp file, either when exiting normally,
	// or if Ctrl+C is pressed.
	sigs := make(chan os.Signal, 1)
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Add(1)
	go func() {
		defer wg.Done()
		sig, signaled := <-sigs
		if signaled {
			fmt.Fprintf(os.Stderr, "\n")
		}
		fmt.Fprintf(os.Stderr, "Removing %q\n", f.Name())
		_ = f.Close()
		_ = os.Remove(f.Name())
		if signaled {
			os.Exit(128 + int(sig.(syscall.Signal)))
		}
	}()
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer close(sigs)
	defer signal.Reset()

	logf("Extracting squashed image to %q", f.Name())
	progress := &progressWriter{}
	if err := writeSquashedTarball(io.MultiWriter(f, progress), img); err != nil {
		return fmt.Errorf("extract squashed image to %q: %w", f.Name(), err)
	}
	progress.Print()

	// Build a new image from scratch
	flat := empty.Image
	logf("Computing layer digest")
	layer, err := tarball.LayerFromFile(f.Name())
	if err != nil {
		return fmt.Errorf("read squashed layer: %w", err)
	}
	flat, err = mutate.AppendLayers(flat, layer)
	if err != nil {
		return fmt.Errorf("append squashed layer to empty image: %w", err)
	}
	diffID, err := layer.DiffID()
	if err != nil {
		return fmt.Errorf("get layer digest: %w", err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return fmt.Errorf("get config file: %w", err)
	}
	cfg = shallowCopy(cfg)
	cfg.RootFS.DiffIDs = []v1.Hash{diffID}
	cfg.History = nil
	cfg.Created = v1.Time{Time: time.Now()}
	flat, err = mutate.ConfigFile(flat, cfg)
	if err != nil {
		return fmt.Errorf("set config file: %w", err)
	}

	// Write image to output file
	logf("Writing image to %q", outputPath)
	out, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create output file: %w", err)
	}
	defer out.Close()
	progress = &progressWriter{}
	if err := tarball.Write(outRef, flat, io.MultiWriter(out, progress)); err != nil {
		return fmt.Errorf("write image to %q: %w", outputPath, err)
	}
	progress.Print()
	return nil
}

func writeSquashedTarball(w io.Writer, img v1.Image) error {
	rc := mutate.Extract(img)
	defer rc.Close()
	_, err := io.Copy(w, rc)
	return err
}

func shallowCopy[T any](v *T) *T {
	clone := *v
	return &clone
}

type progressWriter struct {
	total       int64
	written     int64
	printedOnce bool
	lastPrinted time.Time
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.written += int64(len(p))
	if !*quiet && isatty.IsTerminal(os.Stderr.Fd()) && time.Since(w.lastPrinted) > 100*time.Millisecond {

		w.print()
	}
	return len(p), nil
}

func (w *progressWriter) Print() {
	w.print()
}

func (w *progressWriter) print() {
	if w.printedOnce {
		// Go up one line, clear the line, and go back to the start of the line
		fmt.Fprintf(os.Stderr, "\033[1A\033[K\r")
	}
	fmt.Fprintf(os.Stderr, "Wrote %s\n", humanize.Bytes(uint64(w.written)))
	w.printedOnce = true
	w.lastPrinted = time.Now()
}
