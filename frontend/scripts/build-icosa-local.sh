#!/bin/bash
# Run in a clean scratch checkout on the Studio. Never launches/installs the GUI.
set -euo pipefail
cd "$(dirname "$0")/../.."
version=$(node -p 'require("./frontend/package.json").version')
destination="/Volumes/aofiles/ao-builds/icosa-orchestrator/$version"
refuse() { echo "Version $version already built or reserved: $1. Bump the version; never overwrite artifacts." >&2; exit 1; }
[ ! -e "$destination" ] && [ ! -L "$destination" ] || refuse "$destination"
for artifact in frontend/out/local/*"$version"*; do
 [ ! -e "$artifact" ] && [ ! -L "$artifact" ] || refuse "$artifact"
done
# A manifest records a completed build even if its archives were moved away.
if [ -e frontend/out/local/manifest.json ] || [ -L frontend/out/local/manifest.json ]; then
 node -e 'const m=require("./frontend/out/local/manifest.json"); if (m.version === process.argv[1]) process.exit(1)' "$version" || refuse frontend/out/local/manifest.json
fi
[ "$(uname -m)" = arm64 ] || { echo 'arm64 Studio required' >&2; exit 1; }
[ -z "$(git status --porcelain --untracked-files=no)" ] || { echo 'Tracked source must be clean' >&2; exit 1; }
mkdir -p frontend/out/local
lock="$PWD/frontend/out/local/.build-$version.lock"
mkdir "$lock" || refuse "$lock"
trap 'rmdir "$lock"' EXIT
export APPLE_SIGNING_IDENTITY='Developer ID Application: Markus Frommherz (L98MZWWR98)'
export AO_RELEASE_REPO='Icosa2050/agent-orchestrator'
unset CSC_LINK CSC_KEY_PASSWORD APPLE_API_KEY APPLE_API_KEY_ID APPLE_API_ISSUER AO_PUBLISHED_BUILD SENTRY_AUTH_TOKEN
profile=${AO_NOTARY_PROFILE:-icosa-orchestrator-notary}
unset AO_NOTARY_PROFILE
if xcrun notarytool history --keychain-profile "$profile" > notary-profile-check.log 2>&1; then
 export AO_NOTARY_PROFILE="$profile"
else
 echo 'Notarization is required; notary profile unavailable. See notary-profile-check.log.' >&2
 exit 1
fi
(cd packages/product-ui && nice -n 10 npm ci --no-audit --no-fund)
cd frontend
nice -n 10 npm ci --no-audit --no-fund
nice -n 10 npm run package -- --platform=darwin --arch=arm64
app="$PWD/out/Icosa Orchestrator-darwin-arm64/Icosa Orchestrator.app"
[ -d "$app" ] || { echo "Missing $app" >&2; exit 1; }
mkdir -p out/local
zip="$PWD/out/local/Icosa Orchestrator-$version-arm64.zip"
dmg="$PWD/out/local/Icosa Orchestrator-$version-arm64.dmg"
codesign --verify --deep --strict --verbose=2 "$app"
signature=$(codesign -dvvv "$app" 2>&1)
printf '%s\n' "$signature" | grep -F "Identifier=com.icosa2050.orchestrator"
printf '%s\n' "$signature" | grep -F "TeamIdentifier=L98MZWWR98"
printf '%s\n' "$signature" | grep -E "flags=.*runtime"
ditto -c -k --sequesterRsrc --keepParent "$app" "$zip"
nice -n 10 node scripts/build-mac-dmg.mjs --app "$app" --arch arm64 --output "$dmg"
codesign --force --sign "$APPLE_SIGNING_IDENTITY" --timestamp "$dmg"
xcrun notarytool submit "$dmg" --keychain-profile "$AO_NOTARY_PROFILE" --wait
xcrun stapler staple "$dmg"
bash scripts/verify-mac-artifact.sh "$zip"
bash scripts/verify-mac-artifact.sh "$dmg"
# Verification is read-only; canonical artifacts must be notarized.
{
 codesign -dvvv "$app" 2>&1
 codesign -d -r- "$app" 2>&1
 /usr/libexec/PlistBuddy -c Print "$app/Contents/Info.plist"
 find "$app/Contents" -name Info.plist -path '*Helper*' -print -exec /usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' {} \;
 while IFS= read -r -d '' binary; do
  if file -b "$binary" | grep -q 'Mach-O'; then
   echo "--- $binary"
   codesign --verify --strict --verbose=2 "$binary"
   codesign -dvvv "$binary" 2>&1 | grep -F "TeamIdentifier=L98MZWWR98"
   codesign -dvvv --entitlements :- "$binary" 2>&1
  fi
 done < <(find "$app/Contents" -type f -print0)
 spctl -a -vv -t exec "$app" 2>&1 || true
 spctl -a -vv -t open --context context:primary-signature "$dmg" 2>&1 || true
 xcrun stapler validate "$app" 2>&1 || true
 xcrun stapler validate "$dmg" 2>&1 || true
 cat "$app/Contents/Resources/app-update.yml"
 "$app/Contents/Resources/daemon/ao" version
} > out/local/verification.log
python3 - "$app" "$zip" "$dmg" "$(git rev-parse HEAD)" "$version" <<'PY'
import hashlib,json,os,plistlib,sys,subprocess
app,archive,dmg,commit,version=sys.argv[1:]
with open(app+'/Contents/Info.plist','rb') as f: p=plistlib.load(f)
assert p['CFBundleIdentifier']=='com.icosa2050.orchestrator'
assert p['CFBundleExecutable']=='icosa-orchestrator'
assert p['CFBundleName']=='Icosa Orchestrator'
assert p['CFBundleDisplayName']=='Icosa Orchestrator'
assert [s for item in p['CFBundleURLTypes'] for s in item['CFBundleURLSchemes']]==['icosa-orchestrator']
assert p['CFBundleShortVersionString']==version
assert p['CFBundleVersion']==version
cli=subprocess.check_output([app+'/Contents/Resources/daemon/ao','version'],text=True).strip()
assert cli.split()[0]==version,cli
for root,dirs,files in os.walk(app+'/Contents'):
 if 'Info.plist' in files and root != app+'/Contents' and root.endswith('.app/Contents'):
  with open(root+'/Info.plist','rb') as f: nested=plistlib.load(f)
  assert nested['CFBundleIdentifier'].startswith('com.icosa2050.orchestrator.'),nested
dr=subprocess.run(['codesign','-d','-r-',app],capture_output=True,text=True,check=True)
requirement=dr.stdout+dr.stderr
artifacts=[{'path':f,'bytes':os.path.getsize(f),'sha256':hashlib.file_digest(open(f,'rb'),'sha256').hexdigest()} for f in [archive,dmg]]
with open('out/local/manifest.json','w') as f:json.dump(dict(commit=commit,version=version,arch='arm64',team='L98MZWWR98',identifier=p['CFBundleIdentifier'],profileVersion=1,designatedRequirement=requirement,notaryProfileConfigured=bool(os.environ.get('AO_NOTARY_PROFILE')),artifacts=artifacts),f,indent=2)
PY
# Reserve atomically before copying: another build may have finished meanwhile.
if mkdir -p "$(dirname "$destination")" 2>/dev/null && mkdir "$destination" 2>/dev/null; then
 if cp "$zip" "$dmg" out/local/manifest.json out/local/verification.log "$destination/"; then
  echo "Copied to $destination"
 else
  echo "Manual Terminal-context copy required: $destination" >&2
 fi
elif [ -e "$destination" ] || [ -L "$destination" ]; then
 refuse "$destination"
else
 echo "Manual Terminal-context copy required: $destination" >&2
fi
printf 'Artifacts remain in %s/out/local\n' "$PWD"
