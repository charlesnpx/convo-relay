#!/usr/bin/env bash
set -euo pipefail

NAME="convo-relay"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
VERSION="${CONVO_RELAY_VERSION:-$(git -C "$repo_root" describe --tags --exact-match 2>/dev/null || true)}"
VERSION="${VERSION:-dev}"

OPERATION=""
TARGET="all"
JSON="false"
INSTALL_ROOT=""

die() {
	printf 'convo-relay installer: %s\n' "$*" >&2
	exit 2
}

set_operation() {
	local operation="$1"
	if [[ -n "$OPERATION" ]]; then
		die "exactly one operation flag is allowed"
	fi
	OPERATION="$operation"
}

while [[ $# -gt 0 ]]; do
	case "$1" in
		--plan)
			set_operation "plan"
			shift
			;;
		--install)
			set_operation "install"
			shift
			;;
		--uninstall)
			set_operation "uninstall"
			shift
			;;
		--target)
			[[ $# -ge 2 ]] || die "--target requires claude, codex, tools, or all"
			TARGET="$2"
			shift 2
			;;
		--json)
			JSON="true"
			shift
			;;
		--install-root)
			[[ $# -ge 2 ]] || die "--install-root requires an absolute path"
			INSTALL_ROOT="$2"
			shift 2
			;;
		*)
			die "unknown argument: $1"
			;;
	esac
done

[[ "$JSON" == "true" ]] || die "this installer requires --json"

if [[ -z "$OPERATION" ]]; then
	OPERATION="install"
fi

case "$TARGET" in
	all | codex | claude | tools)
		;;
	*)
		die "unsupported target: $TARGET"
		;;
esac

if [[ -n "$INSTALL_ROOT" && "$INSTALL_ROOT" != /* ]]; then
	die "--install-root must be absolute"
fi

canonical_existing_dir() {
	local dir="$1"
	(cd "$dir" && pwd -P)
}

trim_trailing_slash() {
	local path="$1"
	while [[ "$path" != "/" && "$path" == */ ]]; do
		path="${path%/}"
	done
	printf '%s\n' "$path"
}

resolve_root() {
	local root
	if [[ -n "$INSTALL_ROOT" ]]; then
		root="$INSTALL_ROOT"
		if [[ "$OPERATION" == "install" ]]; then
			mkdir -p "$root"
		fi
		if [[ -d "$root" ]]; then
			canonical_existing_dir "$root"
		else
			trim_trailing_slash "$root"
		fi
		return
	fi

	[[ -n "${HOME:-}" ]] || die "HOME is not set"
	[[ "$HOME" == /* ]] || die "HOME must be absolute"
	if [[ -d "$HOME" ]]; then
		canonical_existing_dir "$HOME"
	else
		trim_trailing_slash "$HOME"
	fi
}

ROOT="$(resolve_root)"

tool_path="$ROOT/.local/bin/convo-relay"
share_root="$ROOT/.local/share/convo-relay"
tool_paths=(
	"$tool_path"
	"$share_root/skill/SKILL.md"
	"$share_root/skill/steer/SKILL.md"
	"$share_root/skill/codex/SKILL.md"
	"$share_root/skill/codex/steer/SKILL.md"
)
claude_paths=(
	"$ROOT/.claude/skills/relay/SKILL.md"
	"$ROOT/.claude/skills/relay:steer/SKILL.md"
)
codex_paths=(
	"$ROOT/.codex/skills/relay/SKILL.md"
	"$ROOT/.codex/skills/relay:steer/SKILL.md"
)

include_tools="false"
include_codex="false"
include_claude="false"
case "$TARGET" in
	all)
		include_tools="true"
		include_codex="true"
		include_claude="true"
		;;
	tools)
		include_tools="true"
		;;
	codex)
		include_codex="true"
		;;
	claude)
		include_claude="true"
		;;
esac

json_escape() {
	local value="$1"
	value="${value//\\/\\\\}"
	value="${value//\"/\\\"}"
	value="${value//$'\n'/\\n}"
	value="${value//$'\r'/\\r}"
	value="${value//$'\t'/\\t}"
	printf '%s' "$value"
}

sha_file() {
	if command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
		return
	fi
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
		return
	fi
	die "shasum or sha256sum is required to report installed files"
}

install_skill_file() {
	local source="$1"
	local destination="$2"
	local mode="$3"
	mkdir -p "$(dirname "$destination")"
	install -m "$mode" "$source" "$destination"
}

build_tool() {
	command -v go >/dev/null 2>&1 || die "go executable not found"
	mkdir -p "$(dirname "$tool_path")"
	if ! (cd "$repo_root" && go build -ldflags "-X main.cliVersion=$VERSION" -o "$tool_path" ./cmd/convo-relay); then
		die "failed to build convo-relay"
	fi
}

install_target_files() {
	if [[ "$include_tools" == "true" ]]; then
		build_tool
		install_skill_file "$repo_root/skill/SKILL.md" "${tool_paths[1]}" 0644
		install_skill_file "$repo_root/skill/steer/SKILL.md" "${tool_paths[2]}" 0644
		install_skill_file "$repo_root/skill/codex/SKILL.md" "${tool_paths[3]}" 0644
		install_skill_file "$repo_root/skill/codex/steer/SKILL.md" "${tool_paths[4]}" 0644
	fi
	if [[ "$include_codex" == "true" ]]; then
		install_skill_file "$repo_root/skill/codex/SKILL.md" "${codex_paths[0]}" 0644
		install_skill_file "$repo_root/skill/codex/steer/SKILL.md" "${codex_paths[1]}" 0644
	fi
	if [[ "$include_claude" == "true" ]]; then
		install_skill_file "$repo_root/skill/SKILL.md" "${claude_paths[0]}" 0644
		install_skill_file "$repo_root/skill/steer/SKILL.md" "${claude_paths[1]}" 0644
	fi
}

uninstall_target_files() {
	if [[ "$include_tools" == "true" ]]; then
		rm -f "${tool_paths[@]}"
	fi
	if [[ "$include_codex" == "true" ]]; then
		rm -f "${codex_paths[@]}"
	fi
	if [[ "$include_claude" == "true" ]]; then
		rm -f "${claude_paths[@]}"
	fi
}

emit_file() {
	local path="$1"
	printf '{"path":"%s"' "$(json_escape "$path")"
	if [[ "$OPERATION" == "install" ]]; then
		[[ -f "$path" ]] || die "installed file is missing: $path"
		printf ',"sha256":"%s"' "$(sha_file "$path")"
	fi
	printf '}'
}

emit_target() {
	local target_name="$1"
	shift
	printf '"%s":{"files":[' "$(json_escape "$target_name")"
	local first_file="true"
	local path
	for path in "$@"; do
		if [[ "$first_file" != "true" ]]; then
			printf ','
		fi
		first_file="false"
		emit_file "$path"
	done
	printf ']}'
}

if [[ "$OPERATION" == "install" ]]; then
	install_target_files
elif [[ "$OPERATION" == "uninstall" ]]; then
	uninstall_target_files
fi

printf '{"schema":1,"name":"%s","version":"%s","operation":"%s","kind":"delegated","capabilities":[]' \
	"$(json_escape "$NAME")" "$(json_escape "$VERSION")" "$(json_escape "$OPERATION")"
printf ',"setup":['
if [[ "$include_tools" == "true" ]]; then
	printf '{"kind":"executable","executable":"go","remediation":"Install Go and ensure go is on PATH before installing convo-relay."}'
fi
printf '],"targets":{'

first_target="true"
if [[ "$include_tools" == "true" ]]; then
	if [[ "$first_target" != "true" ]]; then printf ','; fi
	first_target="false"
	emit_target "tools" "${tool_paths[@]}"
fi
if [[ "$include_codex" == "true" ]]; then
	if [[ "$first_target" != "true" ]]; then printf ','; fi
	first_target="false"
	emit_target "codex" "${codex_paths[@]}"
fi
if [[ "$include_claude" == "true" ]]; then
	if [[ "$first_target" != "true" ]]; then printf ','; fi
	first_target="false"
	emit_target "claude" "${claude_paths[@]}"
fi

printf '},"warnings":[]}\n'
