#!/bin/bash
# switchyard.sh: releases of an app that is built in place on a volume, run in two slots, with the
# traffic split between them by a router. It runs inside the Jobs that `switchyard manifests`
# renders, next to its config (switchyard.env) and the app's hooks (hook-<name>):
#
#   deploy    canary: a new release into the idle slot with 1 pod, then STEPS percent of the
#             traffic in turn, each watched for PAUSE seconds with the app's own PAGES requested
#             from the new slot and the old one, then 100 %, then the old slot stops. A failure
#             of the new release before the end sends all traffic back, and the release is not
#             kept. If instead the old release breaks on the migrated database, all traffic moves
#             to the new one at once: back is no way out then.
#   quick     the same build, all pods, 100 % at once, the old slot stops. No steps, no watch.
#   rollback  the release named in the Job (rollback-r<n>-<time>) into the idle slot, 100 % at
#             once, the old slot stops, then the after-rollback hook. The database is not rolled
#             back.
#   reapply   point slots and followers at their releases and write the weights to the router
#             again, after the slots' manifests were applied again: a Job from the CronJob
#             deploy named deploy-reapply-<time>.
#   build, after-rollback   inside the build Job (job.yaml), on one release: the hooks.
#
# Started as `kubectl create job --from=cronjob/deploy deploy-$(date +%s)` (the dashboard does
# the same). The Job's name is the one part the admission policy leaves free, so it carries the
# options: rollback-r<n>-<time> the release to go back to, deploy-drops-<time> (or quick-) lets
# destructive migrations through.
#
# Releases are directories r<n> on the volume RELEASES_PVC. The last KEEP that went live are
# kept, the live one always; the next build removes every other directory. Slots and followers
# mount theirs with subPath on the volume CODE_VOLUME.
#
# State, in the ConfigMap STATE (the dashboard reads it):
#   releases                 the kept releases, newest first, e.g. "r9 r8 r7 r6 r5"
#   next, keep               the number of the next release, KEEP
#   r<n>_meta_<key>          what the build reported (BUILT key=value ...), r<n>_subject (SUBJECT ...),
#   r<n>_built, r<n>_by      when, and canary or quick; r<n>_drops 1 when it ran destructive
#                            migrations on purpose
#   live, a_release, b_release, weight_a, weight_b
#   status step message started finished from to blocked   the last canary deploy; the same keys
#                            with the prefix quick_ for the last quick deploy, rollback_ for the
#                            last rollback
set -Eeuo pipefail   # -E: the ERR trap (back to the old slot) also fires inside functions

HERE=${SWITCHYARD_DIR:-/switchyard}
RUN=""          # prefix of the last run's keys: "" canary, quick_ or rollback_
ALLOW=""        # 1: destructive migrations allowed
TO=""           # a rollback's target, from the Job's name
REAPPLY=""      # 1: the deploy Job was named deploy-reapply-<time>
OLD_BROKE=""    # 1: the old release broke on the migrated database
REQUESTS=0 REDIRECTS=0 SLOWEST=0

k() { kubectl -n "$NAMESPACE" "$@"; }
log() { echo "$(date +%T) $*"; }
other() { [ "$1" = a ] && echo b || echo a; }
slot_name() { [ "$1" = a ] && echo "$SLOT_A" || echo "$SLOT_B"; }   # the Deployment and its Service

json_str() {   # json_str <text>: as a JSON string
    local s=$1
    s=${s//\\/\\\\}; s=${s//\"/\\\"}; s=${s//$'\n'/\\n}; s=${s//$'\r'/\\r}; s=${s//$'\t'/\\t}
    printf '"%s"' "$s"
}

state() { k get configmap "$STATE" -o go-template="{{index .data \"$1\"}}" | sed 's/^<no value>$//'; }
set_state() {   # key=value ...
    local kv body=""
    for kv in "$@"; do body+="$(json_str "${kv%%=*}"):$(json_str "${kv#*=}"),"; done
    k patch configmap "$STATE" --type merge -p "{\"data\":{${body%,}}}" >/dev/null
}
# shellcheck disable=SC2016 # a go-template, not shell
state_keys() { k get configmap "$STATE" -o go-template='{{range $k, $v := .data}}{{$k}}{{"\n"}}{{end}}'; }
drop_state() {   # key ...
    local key body=""
    for key in "$@"; do body+="$(json_str "$key"):null,"; done
    k patch configmap "$STATE" --type merge -p "{\"data\":{${body%,}}}" >/dev/null
}

# job_options <job name>: what the Job's name says, in TO, ALLOW and REAPPLY
job_options() {
    TO=$(sed -nE "s/^${JOB_PREFIX}rollback-(r[0-9]+)-[0-9]+\$/\1/p" <<<"$1")
    if [[ $1 =~ ^${JOB_PREFIX}(deploy|quick)-drops-[0-9]+$ ]]; then ALLOW=1; fi
    if [[ $1 =~ ^${JOB_PREFIX}deploy-reapply-[0-9]+$ ]]; then REAPPLY=1; fi
}

curl_app() {   # curl_app <slot> <path> [curl options]: the app in that slot, as the router sends it
    local slot=$1 path=$2
    shift 2
    local hdr=()
    [ -z "$HOST" ] || hdr+=(-H "Host: $HOST")
    [ -z "$FORWARDED_PROTO" ] || hdr+=(-H "X-Forwarded-Proto: $FORWARDED_PROTO")
    curl "${hdr[@]}" "$@" "http://$(slot_name "$slot"):$PORT$path"
}

healthy() {   # healthy <slot> <pods>: enough ready pods, and the app answers through its Service
    local ready
    ready=$(k get deploy "$(slot_name "$1")" -o jsonpath='{.status.readyReplicas}')
    [ "${ready:-0}" -ge "$2" ] && curl_app "$1" "$HEALTH" -fsS -m 5 -o /dev/null
}

probe() {   # probe <slot>: one round of PAGES; fails at the first answer of 400 or more or a timeout
    local page out code took
    for page in $PAGES; do
        out=$(curl_app "$1" "$page" -sS -m 10 -o /dev/null -w '%{http_code} %{time_total}' 2>/dev/null) || out="000 10"
        read -r code took <<<"$out"
        REQUESTS=$((REQUESTS + 1))
        [ "$code" -lt 400 ] || { log "slot $1: $page answered $code"; return 1; }
        [ "$code" -lt 300 ] || REDIRECTS=$((REDIRECTS + 1))
        [ "${took%.*}" -lt "$SLOWEST" ] || SLOWEST=${took%.*}
    done
}

# watch_slot <slot> <pods> <seconds> [<old slot>]: PAGES round after round from the slot (and the
# old one) until the time is up. Returns 1 when the slot fails, 2 when the old slot fails.
watch_slot() {
    local end=$((SECONDS + $3))
    REQUESTS=0 REDIRECTS=0 SLOWEST=0
    while [ $SECONDS -lt $end ]; do
        healthy "$1" "$2" || { log "slot $1 is not healthy"; return 1; }
        probe "$1" || return 1
        if [ -n "${4:-}" ]; then probe "$4" || { log "the old release in slot $4 broke"; return 2; }; fi
    done
    log "slot $1${4:+ and $4}: $REQUESTS requests, no errors, $REDIRECTS redirects, slowest about ${SLOWEST}s"
}

scale() { k scale deploy "$(slot_name "$1")" --replicas="$2" >/dev/null; }

describe() {   # describe <release>: its meta, e.g. "commit 1a2b3c4"
    local keys key out=""
    keys=$(state_keys | sed -n "s/^${1}_meta_//p")
    for key in $keys; do out+="$key $(state "${1}_meta_$key"), "; done
    echo "${out%, }"
}

release_value() {   # release_value <release>: RELEASE_VALUE with {id} and {<meta key>} filled in
    local out=$RELEASE_VALUE key
    out=${out//\{id\}/$1}
    while [[ $out =~ \{([A-Za-z0-9_-]+)\} ]]; do
        key=${BASH_REMATCH[1]}
        out=${out//\{$key\}/$(state "${1}_meta_$key")}
    done
    echo "$out"
}

podspec() { case $1 in cronjob/*) echo spec.jobTemplate.spec.template.spec ;; *) echo spec.template.spec ;; esac; }

# mount_release <kind/name> <release>: every mount of the volume CODE_VOLUME in the object's pod
# gets the release as its subPath (and RELEASE_ENV its value), from the next pod on
mount_release() {
    local obj=$1 rel=$2 spec path ops="" c=0 j line m
    spec=$(podspec "$obj")
    path=/${spec//.//}
    while read -r line; do
        j=0
        for m in $line; do
            [ "$m" != "$CODE_VOLUME" ] || ops+="{\"op\":\"add\",\"path\":\"$path/containers/$c/volumeMounts/$j/subPath\",\"value\":\"$rel\"},"
            j=$((j + 1))
        done
        c=$((c + 1))
    done < <(k get "$obj" -o jsonpath="{range .$spec.containers[*]}{range .volumeMounts[*]}{.name}{\" \"}{end}{\"\\n\"}{end}")
    [ -n "$ops" ] || { log "$obj mounts no volume $CODE_VOLUME"; return 1; }
    k patch "$obj" --type json -p "[${ops%,}]" >/dev/null
    [ -z "$RELEASE_ENV" ] || k set env "$obj" "$RELEASE_ENV=$(release_value "$rel")" >/dev/null
}

point() {   # point <slot> <release>: the slot runs that release from its next pod on
    mount_release "deploy/$(slot_name "$1")" "$2"
    set_state "${1}_release=$2"
}

followers_on() {   # followers_on <slot>: the followers run that slot's release from their next pod on
    local obj rel
    rel=$(state "${1}_release")
    for obj in $FOLLOWERS; do mount_release "$obj" "$rel"; done
}

route() {   # route <slot> <weight> [<slot> <weight>]: the router's weights, by slot
    if [ $# -gt 2 ]; then router_set "$(slot_name "$1")" "$2" "$(slot_name "$3")" "$4"
    else router_set "$(slot_name "$1")" "$2"; fi
}

start_slot() {   # start_slot <slot> <pods>: scale and wait until they are ready and reachable
    scale "$1" "$2"
    k rollout status deploy "$(slot_name "$1")" --timeout=300s >/dev/null
    # a pod is Ready (the kubelet's probe gets through) a few seconds before other pods reach it
    # through a NetworkPolicy, on some network plugins: wait for the real path, up to 60 s
    local end=$((SECONDS + 60))
    until healthy "$1" "$2" 2>/dev/null; do
        [ $SECONDS -lt $end ] || { log "slot $1 is ready but not reachable through its Service"; return 1; }
        sleep 3
    done
}

job() {   # job <mode> <release> [<copy from> <keep>]: a job.yaml Job, its log shown here; JOB is its name
    local status=""
    JOB="${JOB_PREFIX}$1-$2-$(date +%s)"
    sed -e "s/@NAME@/$JOB/" -e "s/@MODE@/$1/" -e "s/@RELEASE@/$2/g" -e "s/@FROM@/${3:-}/" \
        -e "s/@KEEP@/${4:-$2}/" -e "s/@ALLOW@/$ALLOW/" "$HERE/job.yaml" | k apply -f - >/dev/null
    until k logs -c build "job/$JOB" >/dev/null 2>&1; do sleep 2; done
    k logs -c build -f "job/$JOB" | sed 's/^/  | /'
    while [ -z "$status" ]; do
        status=$(k get job "$JOB" -o jsonpath='{.status.conditions[?(@.status=="True")].type}' | grep -o -E 'Complete|Failed' || true)
        sleep 2
    done
    [ "$status" = Complete ]
}

# build <release> <copy from>: the build Job, then the release's keys from its log:
# "BUILT key=value ..." lines (r<n>_meta_<key>) and the last "SUBJECT <text>" line
build() {
    local line kv subject blocked meta=()
    # the kept releases, both slots' and the new one stay; every other directory goes
    if ! job build "$1" "$2" "$1 $(state releases) $(state a_release) $(state b_release)"; then
        k logs -c build "job/$JOB" >/tmp/build.log 2>/dev/null || true
        blocked=$(grep '^BLOCKED ' /tmp/build.log | cut -d' ' -f2- || true)
        [ -z "$blocked" ] || set_state "${RUN}blocked=$blocked"
        return 1
    fi
    # parse a saved copy: grep -m1/head on a live pipe end it early, and pipefail calls that an error
    k logs -c build "job/$JOB" >/tmp/build.log
    if grep -q '^destructive migrations allowed' /tmp/build.log; then set_state "${1}_drops=1"; fi
    while read -r line; do
        for kv in ${line#BUILT }; do
            if [[ $kv =~ ^[A-Za-z0-9_-]+=. ]]; then meta+=("${1}_meta_$kv"); fi
        done
    done < <(grep '^BUILT ' /tmp/build.log || true)
    subject=$(grep '^SUBJECT ' /tmp/build.log | tail -1 | cut -d' ' -f2- || true)
    set_state "${meta[@]}" "${1}_subject=$subject" "${1}_built=$(date -Is)"
}

new_release() {   # the next release's name, counted up
    local n
    n=$(state next)
    n=${n:-1}
    set_state next=$((n + 1)) keep="$KEEP"
    echo "r$n"
}

# went_live <release> <by>: into the kept releases, newest first: it, the live one and the
# newest others, KEEP in all. Every key of a release that is not kept goes.
went_live() {
    local list=" $1 " rel live_rel key stale=()
    set_state "${1}_by=$2"
    live_rel=$(state "$(state live)_release")
    [[ -z $live_rel || $list == *" $live_rel "* ]] || list+="$live_rel "
    for rel in $(state releases); do
        [ "$(wc -w <<<"$list")" -lt "$KEEP" ] || break
        [[ $list == *" $rel "* ]] || list+="$rel "
    done
    list=$(tr ' ' '\n' <<<"$list" | sed '/^$/d' | sort -t r -k 2 -n -r | tr '\n' ' ')
    set_state releases="${list% }"
    for key in $(state_keys); do
        if [[ $key =~ ^(r[0-9]+)_ && " $list " != *" ${BASH_REMATCH[1]} "* ]]; then stale+=("$key"); fi
    done
    [ ${#stale[@]} -eq 0 ] || drop_state "${stale[@]}"
}

deploy() {
    local live new from rel pct rc
    live=$(state live); live=${live:-a}; new=$(other "$live"); from=$(state "${live}_release")
    rel=$(new_release)
    log "canary deploy: release $rel into slot $new, from ${from:-nothing}${ALLOW:+, destructive migrations allowed}"
    set_state status=running step=build from="$from" to="$rel" started="$(date -Is)" finished="" message="" blocked=""

    trap 'fail "$live" "$new" "$LINENO: $BASH_COMMAND"' ERR
    scale "$new" 0
    build "$rel" "$from"
    REQUESTS=0 REDIRECTS=0 SLOWEST=0
    if [ -n "$from" ]; then probe "$live" || { log "the live release broke on the migrated database"; OLD_BROKE=1; }; fi

    log "starting slot $new with 1 pod, release $rel ($(describe "$rel"))"
    set_state step=start
    point "$new" "$rel"
    start_slot "$new" 1

    for pct in $STEPS; do
        [ -n "$from" ] && [ -z "$OLD_BROKE" ] || break
        log "$pct % of the traffic to slot $new"
        route "$live" $((100 - pct)) "$new" "$pct"
        set_state step="$pct" "weight_$new=$pct" "weight_$live=$((100 - pct))"
        rc=0; watch_slot "$new" 1 "$PAUSE" "$live" || rc=$?
        [ "$rc" -ne 1 ] || fail "$live" "$new" "release $rel failed at $pct %"
        [ "$rc" -ne 2 ] || OLD_BROKE=1
    done

    [ -z "$OLD_BROKE" ] || log "the old release broke on the migrated database: all traffic to slot $new now"
    log "slot $new to $REPLICAS pods, then 100 % of the traffic"
    start_slot "$new" "$REPLICAS"
    route "$new" 100
    followers_on "$new"
    set_state step=100 "weight_$new=100" "weight_$live=0" live="$new"
    rc=0; watch_slot "$new" "$REPLICAS" "$PAUSE" || rc=$?
    [ "$rc" -eq 0 ] || fail "$live" "$new" "release $rel failed at 100 %"
    trap - ERR

    went_live "$rel" canary
    log "slot $live drains for ${DRAIN}s, then stops"
    sleep "$DRAIN"
    scale "$live" 0
    set_state status=ok step=done finished="$(date -Is)" message="release $rel live: $(describe "$rel")${OLD_BROKE:+; $from broke on the migrated database, so the traffic moved over at once}"
    log "done: release $rel is live"
}

quick() {
    local live new from rel
    RUN=quick_
    live=$(state live); live=${live:-a}; new=$(other "$live"); from=$(state "${live}_release")
    rel=$(new_release)
    log "quick deploy: release $rel into slot $new, from ${from:-nothing}${ALLOW:+, destructive migrations allowed}"
    set_state quick_status=running quick_step=build quick_from="$from" quick_to="$rel" \
        quick_started="$(date -Is)" quick_finished="" quick_message="" quick_blocked=""

    trap 'fail "$live" "$new" "$LINENO: $BASH_COMMAND"' ERR
    scale "$new" 0
    build "$rel" "$from"
    REQUESTS=0 REDIRECTS=0 SLOWEST=0
    if [ -n "$from" ]; then probe "$live" || { log "the live release broke on the migrated database"; OLD_BROKE=1; }; fi

    log "starting slot $new with $REPLICAS pods, release $rel ($(describe "$rel"))"
    set_state quick_step=start
    point "$new" "$rel"
    start_slot "$new" "$REPLICAS"

    log "all traffic to slot $new"
    set_state quick_step=switch
    route "$new" 100
    followers_on "$new"
    set_state live="$new" "weight_$new=100" "weight_$live=0"
    trap - ERR

    went_live "$rel" quick
    scale "$live" 0
    set_state quick_status=ok quick_step=done quick_finished="$(date -Is)" \
        quick_message="release $rel live: $(describe "$rel")${OLD_BROKE:+; $from broke on the migrated database}"
    log "done: release $rel is live"
}

fail() {   # back to the old slot, whatever step we were in
    local live=$1 new=$2 why
    trap - ERR
    log "FAILED at line $3"
    if [ -n "$(state "${live}_release")" ]; then
        log "all traffic back to slot $live"
        start_slot "$live" "$REPLICAS" || true
        route "$live" 100 || true
        followers_on "$live" || true
    fi
    scale "$new" 0
    why="failed in step $(state "${RUN}step")"
    [ -z "$(state "${RUN}blocked")" ] || why="stopped before migrating: destructive migrations (Deploy anyway lets them through)"
    set_state "${RUN}status=failed" "${RUN}finished=$(date -Is)" live="$live" "weight_$live=100" "weight_$new=0" \
        "${RUN}message=$why, $(state "${live}_release") still live"
    exit 1
}

rollback() {
    local live idle from
    RUN=rollback_
    live=$(state live); idle=$(other "$live"); from=$(state "${live}_release")
    set_state rollback_status=running rollback_step=start rollback_from="$from" rollback_to="$TO" \
        rollback_started="$(date -Is)" rollback_finished="" rollback_message=""
    if [ -z "$TO" ] || [[ " $(state releases) " != *" $TO "* ]] || [ "$TO" = "$from" ]; then
        log "nothing to roll back to: job $JOBNAME names no kept release other than the live one ($(state releases))"
        set_state rollback_status=failed rollback_finished="$(date -Is)" rollback_message="no kept release in job $JOBNAME"
        exit 1
    fi

    trap 'rollback_failed "$idle" "$LINENO: $BASH_COMMAND"' ERR
    log "rollback: release $TO ($(describe "$TO")) into slot $idle, then all traffic"
    scale "$idle" 0
    point "$idle" "$TO"
    start_slot "$idle" "$REPLICAS"
    set_state rollback_step=switch
    route "$idle" 100
    followers_on "$idle"
    set_state live="$idle" "weight_$idle=100" "weight_$live=0"
    trap - ERR
    scale "$live" 0

    if [ -f "$HERE/hook-after-rollback" ]; then
        set_state rollback_step=after
        job after-rollback "$TO" || log "the after-rollback hook failed"
    fi
    set_state rollback_status=ok rollback_step=done rollback_finished="$(date -Is)" \
        rollback_message="release $TO live again: $(describe "$TO")"
    log "done: release $TO is live"
}

rollback_failed() {   # nothing switched yet: the old slot keeps the traffic
    trap - ERR
    log "FAILED at line $2"
    scale "$1" 0
    set_state rollback_status=failed rollback_finished="$(date -Is)" \
        rollback_message="failed in step $(state rollback_step), the live release is unchanged"
    exit 1
}

reapply() {
    local slot live
    for slot in a b; do
        [ -z "$(state "${slot}_release")" ] || point "$slot" "$(state "${slot}_release")"
    done
    live=$(state live)
    scale "$live" "$REPLICAS"
    route a "$(state weight_a)" b "$(state weight_b)"
    followers_on "$live"
    set_state keep="$KEEP"
    log "slots, followers and weights written again: a $(state weight_a), b $(state weight_b)"
}

# --- inside the build Job: one release, mounted at BUILD_MOUNT, the whole volume at /releases ---

hook() {   # hook <name>: runs hook-<name> in the release, if the app has one
    [ -f "$HERE/hook-$1" ] || return 0
    (cd "$BUILD_MOUNT" && bash "$HERE/hook-$1")
}

in_build_job() {
    export HOME=/tmp
    if [ "$1" = after-rollback ]; then
        echo "== after-rollback hook on $RELEASE"
        hook after-rollback
        return
    fi

    echo "== releases"
    [[ " $KEEP_RELEASES " == *" $RELEASE "* ]] || { echo "KEEP_RELEASES ($KEEP_RELEASES) lacks $RELEASE itself, refusing to tidy"; exit 1; }
    local dir
    for dir in /releases/*/; do
        [ -d "$dir" ] || continue
        dir=${dir%/}
        [[ " $KEEP_RELEASES " == *" ${dir##*/} "* ]] || { echo "removing ${dir##*/}"; rm -rf "$dir"; }
    done
    # a new release starts as a copy of the live one, so the hooks only add what changed
    if [ -n "$COPY_FROM" ] && [ -d "/releases/$COPY_FROM" ] && [ -z "$(ls -A "$BUILD_MOUNT")" ]; then
        echo "$RELEASE starts as a copy of $COPY_FROM"
        cp -a "/releases/$COPY_FROM/." "$BUILD_MOUNT"
    fi

    if [ -f "$HERE/hook-prepare" ]; then echo "== prepare"; hook prepare; fi

    # the old release keeps serving on the migrated database (and a rollback goes back to it), so
    # the pending migrations may only add
    if [ -f "$HERE/hook-pending-sql" ]; then
        echo "== pending migrations"
        hook pending-sql >/tmp/pending.sql
        if ! awk -f "$HERE/check-sql.awk" /tmp/pending.sql; then
            if [ "${ALLOW_DESTRUCTIVE:-}" = 1 ]; then
                echo "destructive migrations allowed for this deploy"
            else
                echo "stopped before migrating: the release that is live now would break on them"
                exit 3
            fi
        fi
    fi

    echo "== build"
    hook build
}

main() {
    # shellcheck source=/dev/null
    source "${SWITCHYARD_ENV:-$HERE/switchyard.env}"
    case "${1:-}" in
        build|after-rollback)
            in_build_job "$1" ;;
        deploy|quick|rollback|reapply)
            # shellcheck source=/dev/null
            source "$HERE/router-$ROUTER.sh"
            # the Job's name carries a rollback's target and a deploy's permission for destructive migrations
            JOBNAME=$(k get pod "$HOSTNAME" -o jsonpath='{.metadata.labels.job-name}' 2>/dev/null || true)
            job_options "$JOBNAME"
            if [ "$(k get jobs -l "$JOB_LABEL" -o jsonpath='{range .items[*]}{.status.active}{"\n"}{end}' | grep -c 1)" -gt 1 ]; then
                log "another deploy or rollback is running"; exit 1
            fi
            if [ "$1" = deploy ] && [ -n "$REAPPLY" ]; then reapply; else "$1"; fi ;;
        *) echo "usage: switchyard.sh deploy|quick|rollback|reapply|build|after-rollback" >&2; exit 2 ;;
    esac
}

[[ ${BASH_SOURCE[0]} != "$0" ]] || main "$@"
