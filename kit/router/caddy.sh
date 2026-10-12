#!/bin/bash
# The Caddy router adapter, sourced by switchyard.sh. Every adapter defines one function:
#
#   router_set <service> <weight> [<service> <weight>]
#       all of the app's traffic to these Services (the slots), by weight, 100 in all; a Service
#       with weight 0 gets none. Returns non-zero when the router refused, and then the traffic
#       must still flow as before.
#
# Caddy reads its Caddyfile from the ConfigMap CADDY_CONFIGMAP (namespace/name, key CADDY_KEY).
# In the app's reverse_proxy, a block between "# BEGIN <CADDY_BLOCK>" and "# END <CADDY_BLOCK>"
# is rewritten here, loaded into Caddy through its admin API (CADDY_ADMIN; Caddy refuses a bad
# config and keeps the old one), then stored in the ConfigMap. With CADDY_COOKIE a visitor stays
# on one slot (lb_policy cookie, the weights as its fallback); a slot that drops out of the list
# re-assigns its visitors.

router_set() {
    local to="" weights="" file=${TMPDIR:-/tmp}/Caddyfile ns=${CADDY_CONFIGMAP%%/*} cm=${CADDY_CONFIGMAP#*/}
    while [ $# -gt 0 ]; do
        if [ "$2" -gt 0 ]; then to+=" $1.$NAMESPACE.svc.cluster.local:$PORT"; weights+=" $2"; fi
        shift 2
    done
    kubectl -n "$ns" get configmap "$cm" -o go-template="{{index .data \"$CADDY_KEY\"}}" \
        | awk -v name="$CADDY_BLOCK" -v to="$to" -v weights="$weights" -v cookie="$CADDY_COOKIE" '
            $0 ~ "# BEGIN " name "$" {
                ind = $0; sub(/#.*/, "", ind); print
                print ind "to" to
                if (cookie != "") { print ind "lb_policy cookie " cookie " {"; print ind "\tfallback weighted_round_robin" weights; print ind "}" }
                else print ind "lb_policy weighted_round_robin" weights
                skip = 1; next
            }
            $0 ~ "# END " name "$" { skip = 0 }
            !skip' >"$file"
    grep -q "# END $CADDY_BLOCK\$" "$file" || { echo "no block # BEGIN/END $CADDY_BLOCK in $CADDY_CONFIGMAP" >&2; return 1; }
    curl -fsS -X POST -H "Content-Type: text/caddyfile" --data-binary "@$file" "$CADDY_ADMIN/load"
    kubectl -n "$ns" patch configmap "$cm" --type merge \
        -p "{\"data\":{$(json_str "$CADDY_KEY"):$(json_str "$(cat "$file")"$'\n')}}" >/dev/null
}
