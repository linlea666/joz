"""Local IPC health only; Discord availability is separately reported."""
import json
import os
import sys
import time

PATH = '/tmp/collector-health.json'

def healthy(status, now):
    return (status.get('ipc_configured') is True and
            0 <= now - status.get('checked_at', 0) < 20)

def write_status(ipc_configured, state, credential_set, head_event_age_seconds=0.0, delivery_stuck=False):
    # Contains no credentials, events, or account identifiers.
    payload = {'checked_at': time.time(), 'ipc_configured': ipc_configured,
               'discord_state': state, 'credential_set': credential_set,
               'head_event_age_seconds': head_event_age_seconds, 'delivery_stuck': bool(delivery_stuck)}
    temp = PATH + '.tmp'
    with open(temp, 'w') as f:
        json.dump(payload, f)
    os.replace(temp, PATH)

def describe(status):
    state = status.get('discord_state', 'unknown') if status.get('credential_set') else '等待配置（未配置或停用 Discord 凭证）'
    text = 'Collector IPC: healthy; Discord: ' + state
    if status.get('delivery_stuck'):
        # IPC is still healthy (no container restart), but the event queue
        # head has not moved: operators must see it.
        text += '; WARNING 事件队头卡住 %ds (event queue head stuck)' % int(status.get('head_event_age_seconds') or 0)
    return text

if __name__ == '__main__':
    try:
        with open(PATH) as f:
            status = json.load(f)
        if not healthy(status, time.time()):
            sys.exit('Collector IPC unavailable or stale')
        if '--describe' in sys.argv:
            print(describe(status))
    except (OSError, ValueError, TypeError):
        sys.exit('Collector IPC health unavailable')
