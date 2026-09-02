#!/bin/sh

# Tags in this repository are unprefixed (e.g. 1.0.5), so match both styles.
case "$*" in
  *--bare*)
    # Strip the optional "v" prefix
    git describe --tags --match="v[0-9]*\.[0-9]*\.[0-9]*" --match="[0-9]*\.[0-9]*\.[0-9]*" | sed 's/^v//'
    ;;
  *)
    git describe --tags --match="v[0-9]*\.[0-9]*\.[0-9]*" --match="[0-9]*\.[0-9]*\.[0-9]*"
    ;;
esac
