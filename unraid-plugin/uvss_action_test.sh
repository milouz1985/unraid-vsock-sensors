#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
action_path="$script_dir/uvss_action.php"

run_invalid_request() {
    local post_json=$1
    # Keep PHP variables literal; the request data is passed through the environment.
    # shellcheck disable=SC2016
    UVSS_ACTION_PATH="$action_path" UVSS_POST_JSON="$post_json" php \
        -d display_errors=1 -d log_errors=0 -r '
            $_SERVER["REQUEST_METHOD"] = "POST";
            $_POST = json_decode(getenv("UVSS_POST_JSON"), true);
            register_shutdown_function(function (): void {
                echo "|" . http_response_code();
            });
            include getenv("UVSS_ACTION_PATH");
        ' 2>&1
}

result="$(run_invalid_request '{"action":[]}')"
if [[ $result != '{"ok":false,"error":"unknown action"}|400' ]]; then
    echo "malformed action response: $result" >&2
    exit 1
fi

result="$(run_invalid_request '{"action":"set-disk-policy","disk_id":[],"policy":"auto"}')"
if [[ $result != '{"ok":false,"error":"invalid disk policy request"}|400' ]]; then
    echo "malformed policy response: $result" >&2
    exit 1
fi
