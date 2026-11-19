#!/usr/bin/env node
// Fork (Icosa2050): build-time version stamping for build-icosa-local.sh.
//
// frontend/package.json carries only the base convention (<base>-icosa.<floor>,
// e.g. 0.14.8-icosa.2). Do not bump versions by PR; build-icosa stamps them.
// Each build gets <base>-icosa.<N>, N = 1 + the highest N already built for
// that base (aofiles build root and out/local), never below the committed
// floor. An upstream sync that changes the base resets N to 1.
//
//   icosa-version.mjs resolve --package-version V --build-root DIR --local DIR [--requested V]
//     prints "<version> <bundleVersion>"
//   icosa-version.mjs stamp --version V FILE...
//     writes V into package.json / package-lock.json root versions
import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const BASE = /^(\d+)\.(\d+)\.(\d+)$/;
// N is capped so the numeric CFBundleVersion below stays monotonic.
export const MAX_BUILD_NUMBER = 999;

const escape = (text) => text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

/** Splits the committed package.json version into its base and floor N. */
export function icosaBase(packageVersion) {
	const match = /^(\d+\.\d+\.\d+)(?:-icosa\.(0|[1-9]\d*))?$/.exec(packageVersion);
	if (!match) throw new Error(`package.json version ${packageVersion} is not <major>.<minor>.<patch>[-icosa.<N>]`);
	return { base: match[1], floor: Number(match[2] ?? 0) };
}

/** N of an exact <base>-icosa.<N> version, or null for anything else. */
export function parseIcosaVersion(version, base) {
	const match = new RegExp(`^${escape(base)}-icosa\\.([1-9]\\d*)$`).exec(version);
	return match ? Number(match[1]) : null;
}

/** Every N for this base mentioned in folder or artifact names. */
export function buildNumbers(names, base) {
	const pattern = new RegExp(`(?<![\\d.])${escape(base)}-icosa\\.(\\d+)(?!\\d)`, "g");
	const numbers = [];
	for (const name of names) for (const match of name.matchAll(pattern)) numbers.push(Number(match[1]));
	return numbers;
}

/** Monotonic numeric CFBundleVersion: <major>.<minor>.<patch * 1000 + N>. */
export function bundleVersion(version) {
	const match = /^(\d+)\.(\d+)\.(\d+)-icosa\.([1-9]\d*)$/.exec(version);
	if (!match) throw new Error(`${version} is not <base>-icosa.<N>`);
	const [, major, minor, patch, n] = match.map(Number);
	if (n > MAX_BUILD_NUMBER) throw new Error(`build number ${n} exceeds ${MAX_BUILD_NUMBER}; CFBundleVersion would stop being monotonic`);
	return `${major}.${minor}.${patch * 1000 + n}`;
}

/** Picks the requested version (validated) or the next free one. */
export function resolveIcosaVersion({ packageVersion, names, requested }) {
	const { base, floor } = icosaBase(packageVersion);
	let version;
	if (requested) {
		if (parseIcosaVersion(requested, base) === null) {
			throw new Error(`ICOSA_VERSION ${requested} must match ^${base}-icosa\\.<N>$ (N >= 1, base from package.json)`);
		}
		version = requested;
	} else {
		version = `${base}-icosa.${Math.max(floor, ...buildNumbers(names, base)) + 1}`;
	}
	return { version, base, bundleVersion: bundleVersion(version) };
}

/** Names an auto-numbered build must not collide with. */
export function existingNames({ buildRoot, local, requireBuildRoot }) {
	const names = [];
	try {
		names.push(...readdirSync(buildRoot));
	} catch (error) {
		if (requireBuildRoot) throw new Error(`cannot list ${buildRoot} (${error.code ?? error.message}); auto-numbering needs it. Mount aofiles or set ICOSA_VERSION`);
	}
	try {
		names.push(...readdirSync(local));
	} catch {}
	try {
		names.push(String(JSON.parse(readFileSync(path.join(local, "manifest.json"), "utf8")).version));
	} catch {}
	return names;
}

/** Writes the stamped version into package.json and package-lock.json. */
export function stampVersion(file, version) {
	const json = JSON.parse(readFileSync(file, "utf8"));
	json.version = version;
	if (json.packages?.[""]) json.packages[""].version = version;
	writeFileSync(file, `${JSON.stringify(json, null, "\t")}\n`);
}

function option(args, name) {
	const index = args.indexOf(name);
	return index >= 0 ? args[index + 1] : undefined;
}

function main(argv) {
	const [command, ...args] = argv;
	if (command === "resolve") {
		const requested = option(args, "--requested") || undefined;
		const names = existingNames({ buildRoot: option(args, "--build-root"), local: option(args, "--local"), requireBuildRoot: !requested });
		const resolved = resolveIcosaVersion({ packageVersion: option(args, "--package-version"), names, requested });
		console.log(`${resolved.version} ${resolved.bundleVersion}`);
	} else if (command === "stamp") {
		const version = option(args, "--version");
		bundleVersion(version);
		for (const file of args.slice(args.indexOf("--version") + 2)) stampVersion(file, version);
	} else {
		throw new Error("usage: icosa-version.mjs resolve|stamp ...");
	}
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
	try {
		main(process.argv.slice(2));
	} catch (error) {
		console.error(`icosa-version: ${error.message}`);
		process.exit(1);
	}
}
