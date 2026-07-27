#!/usr/bin/env node

import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";

const fixturePath = process.argv[2];
if (!fixturePath) throw new Error("fixture path is required");
const fixtures = JSON.parse(readFileSync(fixturePath, "utf8"));

const sha256 = bytes => `sha256:${createHash("sha256").update(bytes).digest("hex")}`;

function normalizeNumber(raw) {
  const match = /^(-?)(0|[1-9][0-9]*)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$/.exec(raw);
  if (!match) throw new Error(`invalid JSON number: ${raw}`);
  const digits = match[2] + (match[3] ?? "");
  const first = [...digits].findIndex(character => character !== "0");
  if (first < 0) return "0";
  const significand = digits.slice(first).replace(/0+$/, "");
  const exponent = BigInt(match[4] ?? "0") + BigInt(match[2].length - first - 1);
  const fraction = significand.length > 1 ? `.${significand.slice(1)}` : "";
  const suffix = exponent === 0n ? "" : `e${exponent}`;
  return `${match[1]}${significand[0]}${fraction}${suffix}`;
}

function canonical(value) {
  if (value === null || typeof value === "boolean") return JSON.stringify(value);
  if (typeof value === "string") {
    return JSON.stringify(value).replaceAll("\u2028", "\\u2028").replaceAll("\u2029", "\\u2029");
  }
  if (typeof value === "number") {
    if (!Number.isFinite(value)) throw new Error("numbers must be finite");
    return normalizeNumber(Object.is(value, -0) ? "-0" : value.toString());
  }
  if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`;
  const keys = Object.keys(value).sort((left, right) => Buffer.compare(Buffer.from(left), Buffer.from(right)));
  return `{${keys.map(key => `${canonical(key)}:${canonical(value[key])}`).join(",")}}`;
}

function withoutPointer(value, pointer) {
  const result = structuredClone(value);
  const segments = pointer.slice(1).split("/").map(segment => segment.replaceAll("~1", "/").replaceAll("~0", "~"));
  let parent = result;
  for (const segment of segments.slice(0, -1)) {
    if (parent?.[segment] === undefined) return result;
    parent = parent[segment];
  }
  if (parent && Object.hasOwn(parent, segments.at(-1))) delete parent[segments.at(-1)];
  return result;
}

let failed = false;
for (const fixture of fixtures.cases) {
  let canonicalBytes;
  if (fixture.class === "raw-bytes") {
    canonicalBytes = fixture.utf8;
  } else {
    let value = fixture.value;
    if (fixture.class === "storage-envelope" && fixture.kind === "execution_workspace") {
      for (const pointer of ["/identity", "/source/git_root", "/source/launch_cwd", "/source_after/git_root", "/source_after/launch_cwd"]) {
        value = withoutPointer(value, pointer);
      }
    }
    canonicalBytes = canonical(value);
    if (fixture.canonical !== canonicalBytes) {
      console.error(`${fixture.id} canonical: ${JSON.stringify(canonicalBytes)}`);
      failed = true;
    }
  }
  const digest = sha256(Buffer.from(canonicalBytes, "utf8"));
  if (fixture.digest !== digest) {
    console.error(`${fixture.id} digest: ${digest}`);
    failed = true;
  }
}

const numberCanonicals = fixtures.equivalent_numbers.json.map(text => canonical(JSON.parse(text)));
if (numberCanonicals.some(value => value !== fixtures.equivalent_numbers.canonical)) {
  console.error(`equivalent_numbers canonical: ${JSON.stringify(numberCanonicals)}`);
  failed = true;
}
const numberDigest = sha256(Buffer.from(numberCanonicals[0], "utf8"));
if (numberDigest !== fixtures.equivalent_numbers.digest) {
  console.error(`equivalent_numbers digest: ${numberDigest}`);
  failed = true;
}

if (failed) process.exitCode = 1;
