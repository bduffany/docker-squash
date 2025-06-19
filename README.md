# docker-squash

Command-line tool to squash a docker image, producing a new image which
only has a single flattened layer.

Only supports single-image manifests for now (multi-arch images are
not yet supported).

## Installation

Install with `go`:

```shell
go install github.com/bduffany/docker-squash@latest
```

## Usage

```
Usage: docker-squash [ OPTIONS ...] SOURCE DEST

SOURCE can be either:
- A local tarball archive path, like "/path/to/image.tar"
- A remote image ref prefixed with "docker://", like "docker://example:foo"

DEST is the output tarball archive path.

Options:
  -quiet
        Don't show progress
  -tag string
        Tag to apply to the image (default "docker-squash-$TIMESTAMP_UNIX_NANOS")
```

### Examples

```shell
# Pull a remote image "example:tag", and produce a flattened tarball
# "example_squashed.tar", tagged with "my-flat-image:tag"
docker-squash -t example-squashed:tag docker://example:tag example_squashed.tar

# Or, if you already have an image tarball (e.g. from 'docker save'),
# pass that instead:
docker-squash -t example-squashed:tag example.tar example_squashed.tar
```
