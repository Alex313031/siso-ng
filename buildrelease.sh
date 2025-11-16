#!/bin/bash

export THIS_FILE_PATH="`readlink -f "$0"`" &&

HERE="`dirname "$THIS_FILE_PATH"`" &&

echo $HERE &&

forceRebuild() {
  go build -C $HERE -o siso-ng -ldflags '-s -w -extldflags "-static"' -a -v "$@"
}


case $1 in
	--force) forceRebuild; exit 0;;
esac

go build -C $HERE -o siso-ng -ldflags '-s -w -extldflags "-static"' -v "$@"
