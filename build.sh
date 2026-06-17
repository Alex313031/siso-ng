#!/bin/bash

# Copyright (c) 2026 Alex313031

# Abort on errors, unset variables, and failed pipes so a broken build can
# never silently fall through to zipping/cleanup and exit 0.
set -euo pipefail

YEL='\033[1;33m' # Yellow
CYA='\033[1;96m' # Cyan
RED='\033[1;31m' # Red
GRE='\033[1;32m' # Green
c0='\033[0;00m'  # Reset Text
bold='\033[1;37m' # Bold Text
underline='\033[4m' # Underline Text

# Error handling
yell() { echo -e "$0: $*" >&2; }
die()  { yell "${RED}$* ${c0}"; exit 1; }
try() { "$@" || die "${RED}Failed $*"; }

SCRIPTNAME=$(basename "$0")
SCRIPTVER="1.1.0"

export HERE=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )

export RELDIR=${HERE}/release

export CGO_ENABLED=0

WANT_TARGET=""
VFLAG=()
FORCEFLAG=()
WANT_DEBUG=0

# GoBuild <arch> <os> <binpath> <zipname> [extra go build args...]
GoBuild() {
  local target_arch="$1"
  local target_os="$2"
  local binpath="$3"
  local zipname="$4"
  shift 4

  local bin_base
  bin_base=$(basename "${binpath}")

  export GOOS="${target_os}"
  export GOARCH="${target_arch}"
  if [ "$WANT_DEBUG" == "1" ]; then
    zipname+="_debug"
    try go build -C "${HERE}" -o "${binpath}" -gcflags="all=-N -l -dwarf" -ldflags '-extldflags "-static"' "${FORCEFLAG[@]}" "${VFLAG[@]}" "$@"
  else
    try go build -C "${HERE}" -o "${binpath}" -ldflags '-s -w -extldflags "-static"' "${FORCEFLAG[@]}" "${VFLAG[@]}" "$@"
  fi

  # Zip up and delete binary. cd into RELDIR so the archive stores just the
  # binary name rather than its full absolute path. Drop any stale archive
  # first so a rebuild can't leave old entries behind.
  rm -f "${RELDIR}/${zipname}.zip"
  ( cd "${RELDIR}" && zip -q "${zipname}.zip" "${bin_base}" ) \
      || die "${RED}Failed to zip ${bin_base}"
  try rm -f "${VFLAG[@]}" "${binpath}"
}

BuildSiso() {
  local bin_base="$1"
  shift 1

  mkdir -pv "${RELDIR}"

  local target_arch="amd64"
  printf "${GRE}Starting Siso-ng build.${c0}\n"

  if [ "$WANT_TARGET" == "linux" ] || [ "$WANT_TARGET" == "all" ]; then
    printf "${CYA}Building Siso-ng for Linux... ${c0}\n"
    GoBuild "${target_arch}" "linux" "${RELDIR}/${bin_base}" "siso-ng_linux"
    printf "${CYA}Linux build completed. ${c0}\n"
  fi
  if [ "$WANT_TARGET" == "win" ] || [ "$WANT_TARGET" == "all" ]; then
    printf "${CYA}Building Siso-ng for Windows... ${c0}\n"
    GoBuild "${target_arch}" "windows" "${RELDIR}/${bin_base}.exe" "siso-ng_win"
    printf "${CYA}Windows build completed. ${c0}\n"
  fi
  if [ "$WANT_TARGET" == "mac" ] || [ "$WANT_TARGET" == "all" ]; then
    printf "${CYA}Building Siso-ng for MacOS... ${c0}\n"
    GoBuild "${target_arch}" "darwin" "${RELDIR}/${bin_base}" "siso-ng_macos"
    printf "${CYA}MacOS build completed. ${c0}\n"
  fi
}

show_help() {
  cat <<EOF
Usage:
  $SCRIPTNAME [options]

Bash script to build Siso-ng for Linux, Windows, or MacOS.

Options:
  -h, --help    Show this help
  --version     Show script version
  -c, --clean   Remove build artifacts
  -f, --force   Force rebuild
  --deps        Install build dependencies
  -a, --all     Build Siso-ng for all three platforms
  -l, --linux   Build Siso-ng for Linux
  -w, --win     Build Siso-ng for Windows
  -m, --mac     Build Siso-ng for MacOS
  -d, --debug   Make a debug build
  -v, --verbose Verbose build output

EOF

  exit 0
}

show_version() {
  printf "\n ${bold} %s Version %s \n\n" "$SCRIPTNAME" "$SCRIPTVER"
  exit 0
}

install_deps() {
  if ! command -v apt-get >/dev/null; then
    die "--deps only supports apt-based systems (Ubuntu/Debian); install the prerequisites manually"
  fi
  # use sudo only when not already root (e.g. plain CI containers lack sudo).
  # An if-block (not `&& sudo=...`) so the root case doesn't trip `set -e`.
  local sudo=""
  if [ "$(id -u)" -ne 0 ]; then sudo="sudo"; fi

  printf "${GRE}Installing dependencies for %s...${c0}\n" "$SCRIPTNAME"
  $sudo apt-get update || die "apt-get update failed"
  $sudo apt-get install -y golang-go zip \
      || die "Failed to install dependencies"
  printf "${GRE}Done installing dependencies!${c0}\n"
}

clean_out() {
  printf "${YEL}Cleaning build artifacts...${c0}\n"
  rm -fv "${RELDIR}"/siso-ng_*.zip \
         "${RELDIR}"/siso-ng "${RELDIR}"/siso-ng.exe
}

while :; do
  case ${1:-} in
    -h|--help)
        show_help
        ;;
    --version)
        show_version
        ;;
    --deps)
        install_deps
        exit 0
        ;;
    -v|--verbose)
        VFLAG=("-v")
        ;;
    -c|--clean)
        clean_out
        exit 0
        ;;
    -d|--debug)
        WANT_DEBUG=1
        ;;
    -f|--force)
        FORCEFLAG=("-a")
        ;;
    -l|--linux)
        WANT_TARGET="linux"
        ;;
    -w|--win)
        WANT_TARGET="win"
        ;;
    -m|--mac)
        WANT_TARGET="mac"
        ;;
    -a|--all)
        WANT_TARGET="all"
        ;;
    --)
        shift
        break
        ;;
    -?*)
        die "Unknown option '$1'"
        ;;
    *)
        break
  esac
  shift
done

case "$WANT_TARGET" in
  linux|win|mac|all)
      try BuildSiso "siso-ng"
      printf "${GRE}Build(s) completed. Build artifacts are in ${bold}${RELDIR} ${c0}\n"
      ;;
  *)
      yell "${RED}Unsupported build target. \n${c0}"
      show_help
      ;;
esac

exit 0
