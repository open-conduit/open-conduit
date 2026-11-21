#!/bin/sh
# Publishes @open-conduit/sdk to npm at the core's version, so a package
# `extension new --lang ts` starts can depend on it.
#
#   scripts/publish-sdk.sh <version>
#
# The release (.releaserc.json) runs this before GoReleaser, so no binary
# ships naming an SDK version that isn't on npm. It signs in with npm trusted
# publishing, through the release workflow's id-token permission, so there is
# no token to keep; the package's trusted publisher names release.yml.
#
# A version already on npm is left alone, so a retried release gets past it.
# Until the package first exists on npm, a refused publish is a warning, so
# releases keep working before the owner has set the package up. Once it
# exists, any failure stops the release.
set -eu
version=${1:?usage: publish-sdk.sh <version>}
package=@open-conduit/sdk

# Prints "yes" or "no"; any answer but a 404 is an error, not an absence.
on_npm() {
	if output=$(npm view "$1" version 2>&1); then
		[ -n "$output" ] && echo yes || echo no
		return
	fi
	case $output in
	*E404*) echo no ;;
	*)
		printf '%s\n' "$output" >&2
		return 1
		;;
	esac
}

released=$(on_npm "$package@$version")
if [ "$released" = yes ]; then
	echo "$package@$version is already on npm"
	exit 0
fi
exists=$(on_npm "$package")

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
tarball=$(node scripts/pack-sdk.mjs "$version" "$work")

if npm publish "$tarball" --access public; then
	echo "published $package@$version"
elif [ "$exists" = no ]; then
	echo "warning: $package isn't on npm yet and this release couldn't publish it; set up the package and its trusted publisher (see CONTRIBUTING.md)" >&2
else
	echo "publishing $package@$version failed" >&2
	exit 1
fi
