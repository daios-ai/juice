#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
#
# install.sh — install the juice binary.
#
#   curl -fsSL https://juiceos.org/install.sh | sh
#   curl -fsSL https://juiceos.org/install.sh | sh -s -- VERSION
#
# Keep this script simple and easily auditable.
#
# It downloads one released binary, refuses it unless it matches the published checksum, and puts
# it in $JUICE_HOME/bin (default ~/.juice/bin), beside the kernels, logins and keys that already
# live there. Nothing is installed system-wide, nothing asks for a password, and no kernel is
# started: `juice kernel serve` asks which money a new kernel uses and prints a recovery phrase
# once, and a script read from a pipe has no terminal to ask on.
#
# Everything runs inside main, called on the last line, so a connection that breaks midway through
# `curl | sh` cannot leave half of this script executed.

set -eu

REPO="daios-ai/juice"

say()  { printf '%s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
	cat <<'EOF'
Install the juice binary.

Usage:
  install.sh [VERSION] [--no-modify-path]
  curl -fsSL <url>/install.sh | sh -s -- [VERSION] [--no-modify-path]

  VERSION            a release tag from the releases page; the latest release by default
  --no-modify-path   do not touch any shell profile; print what to add instead
  -h, --help         print this help

Environment:
  JUICE_HOME         where juice keeps everything (default ~/.juice); the binary
                     goes in its bin/ subdirectory. Must be an absolute path.
EOF
	exit 0
}

# platform maps this machine to the asset built for it. An unknown one is named rather than guessed:
# the wrong binary is worse than none.
platform() {
	case "$(uname -sm)" in
	"Darwin x86_64") echo "juice_darwin_amd64.tar.gz" ;;
	"Darwin arm64") echo "juice_darwin_arm64.tar.gz" ;;
	"Linux x86_64") echo "juice_linux_amd64.tar.gz" ;;
	"Linux aarch64" | "Linux arm64") echo "juice_linux_arm64.tar.gz" ;;
	*) die "unsupported platform: $(uname -sm). Build from source: https://github.com/$REPO" ;;
	esac
}

# verify refuses anything it cannot prove. A checksum that is skipped when the tool, the file or the
# entry is missing is not a check: anyone able to fail one request would have turned it off.
verify() {
	dir="$1" asset="$2" base="$3"
	if command -v sha256sum >/dev/null 2>&1; then
		sum="sha256sum"
	elif command -v shasum >/dev/null 2>&1; then
		sum="shasum -a 256"
	else
		die "no sha256sum or shasum found; one is needed to check the download."
	fi
	curl -fsSL --proto '=https' -o "$dir/checksums.txt" "$base/checksums.txt" ||
		die "could not download $base/checksums.txt, so the binary cannot be checked."
	want=$(awk -v a="$asset" '$2 == a || $2 == "*"a {print $1}' "$dir/checksums.txt")
	[ -n "$want" ] || die "checksums.txt names no $asset, so the binary cannot be checked."
	got=$(cd "$dir" && $sum "$asset" | awk '{print $1}')
	[ "$want" = "$got" ] || die "checksum mismatch for $asset: expected $want, got $got"
	say "Checksum verified."
}

# put_binary installs only a binary that runs. It lands beside the target on the same filesystem,
# is tried there, and replaces the old one by rename, which is atomic — so a download that turns out
# to be unusable never destroys a working installation.
put_binary() {
	from="$1" exe="$2"
	staged="$exe.new.$$"
	mv "$from" "$staged"
	chmod 755 "$staged"
	if ! version=$("$staged" --version 2>/dev/null); then
		rm -f "$staged"
		die "the downloaded binary does not run on this machine; nothing was changed."
	fi
	mv "$staged" "$exe"
	say "Installed $version to $exe"
}

# add_line appends one line to a shell profile, once. A profile that already mentions the directory
# is left alone rather than given a second copy on every upgrade.
add_line() {
	profile="$1" marker="$2" line="$3"
	if [ -f "$profile" ] && grep -Fqs "$marker" "$profile"; then
		return 1
	fi
	mkdir -p "$(dirname "$profile")"
	printf '\n# added by the juice installer\n%s\n' "$line" >>"$profile"
	return 0
}

main() {
	modify_path=1
	version=""
	for arg in "$@"; do
		case "$arg" in
		-h | --help) usage ;;
		--no-modify-path) modify_path=0 ;;
		-*) die "unknown option: $arg (try --help)" ;;
		*)
			[ -z "$version" ] || die "two versions given: $version and $arg"
			version="$arg"
			;;
		esac
	done

	command -v curl >/dev/null 2>&1 || die "curl is required but not installed."
	command -v tar >/dev/null 2>&1 || die "tar is required but not installed."

	asset=$(platform)
	if [ -n "$version" ]; then
		base="https://github.com/$REPO/releases/download/$version"
	else
		base="https://github.com/$REPO/releases/latest/download"
	fi

	# The home is where every juice program looks for kernels, logins and keys, and each of them
	# resolves it at run time. A relative one would mean a different installation from each
	# directory, so it is refused here as the kernel refuses it.
	juice_home="${JUICE_HOME:-$HOME/.juice}"
	case "$juice_home" in
	/*) ;;
	*) die "JUICE_HOME must be an absolute path, not $juice_home" ;;
	esac
	bin_dir="$juice_home/bin"
	exe="$bin_dir/juice"

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT

	say "Downloading $asset..."
	curl -fsSL --proto '=https' -o "$tmp/$asset" "$base/$asset" ||
		die "could not download $base/$asset"
	verify "$tmp" "$asset" "$base"

	tar -xzf "$tmp/$asset" -C "$tmp" || die "could not unpack $asset"
	[ -f "$tmp/juice" ] || die "$asset does not contain a juice binary"
	mkdir -p "$bin_dir"
	put_binary "$tmp/juice" "$exe"

	# The binary is only usable if the shell finds it, and a non-default home is only used if every
	# later command sees it. Both are one line in the same profile.
	path_line="export PATH=\"$bin_dir:\$PATH\""
	home_line="export JUICE_HOME=\"$juice_home\""
	case "$(basename "${SHELL:-/bin/sh}")" in
	zsh) profile="$HOME/.zshrc" ;;
	bash) profile="$HOME/.bashrc" ;;
	fish)
		profile="$HOME/.config/fish/config.fish"
		path_line="set -gx PATH $bin_dir \$PATH"
		home_line="set -gx JUICE_HOME $juice_home"
		;;
	*) profile="$HOME/.profile" ;;
	esac

	case ":$PATH:" in
	*":$bin_dir:"*) needs_path=0 ;;
	*) needs_path=1 ;;
	esac
	[ "$juice_home" = "$HOME/.juice" ] && needs_home=0 || needs_home=1

	if [ "$modify_path" -eq 1 ]; then
		[ "$needs_path" -eq 1 ] && add_line "$profile" "$bin_dir" "$path_line" &&
			say "Added $bin_dir to PATH in $profile."
		[ "$needs_home" -eq 1 ] && add_line "$profile" "JUICE_HOME" "$home_line" &&
			say "Set JUICE_HOME to $juice_home in $profile, so juice uses that installation."
		if [ "$needs_path" -eq 1 ] || [ "$needs_home" -eq 1 ]; then
			say "Open a new shell for that to take effect."
		fi
	else
		[ "$needs_path" -eq 1 ] && say "$bin_dir is not on your PATH. Add it with:" && say "  $path_line"
		[ "$needs_home" -eq 1 ] &&
			say "juice reads JUICE_HOME at every run, so set it or it will use ~/.juice:" &&
			say "  $home_line"
	fi

	say ""
	say "Create your first kernel with:  juice kernel serve NAME"
	say "It will ask which money that kernel uses, and print a recovery phrase once."
}

main "$@"
