#!/usr/bin/env python3
"""Hermes Google Calendar client; standard library only, persistent private OAuth.

No credentials belong in this repository. Install in $HERMES_HOME alongside
calendar_client_secret.json (a Google Desktop OAuth client).
"""
import argparse
import base64
from datetime import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from zoneinfo import ZoneInfo

HOME = Path(os.environ.get('HERMES_HOME', Path.home() / '.hermes'))
API = 'https://www.googleapis.com/calendar/v3'
TOKEN_URL = 'https://oauth2.googleapis.com/token'
SCOPES = ['https://www.googleapis.com/auth/calendar.events',
          'https://www.googleapis.com/auth/calendar.calendarlist.readonly']
REDIRECT = 'http://127.0.0.1:8765/'


def save_private(path, data):
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temp = tempfile.mkstemp(dir=path.parent)
    try:
        with os.fdopen(fd, 'w') as f:
            json.dump(data, f)
        os.replace(temp, path)
    finally:
        if os.path.exists(temp):
            os.unlink(temp)


def request_json(url, *, method='GET', body=None, headers=None, form=False):
    headers = dict(headers or {})
    data = None
    if body is not None:
        data = (urllib.parse.urlencode(body) if form else json.dumps(body)).encode()
        headers['Content-Type'] = ('application/x-www-form-urlencoded' if form
                                   else 'application/json')
    try:
        with urllib.request.urlopen(urllib.request.Request(
                url, data=data, headers=headers, method=method), timeout=30) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        # Never echo provider bodies, tokens, or event contents in errors.
        hints = {401: 'authorization expired; run auth-url',
                 403: 'Calendar permission missing or calendar is read-only',
                 404: 'calendar or event not found',
                 409: 'event ID already exists; get it before retrying',
                 412: 'event changed; read it again before applying changes'}
        raise RuntimeError(f'Google HTTP {e.code}: {hints.get(e.code, "request failed")}') from None


def client_config():
    data = json.loads((HOME / 'calendar_client_secret.json').read_text())
    if 'installed' not in data:
        raise ValueError('A Desktop OAuth client is required')
    return data['installed']


def auth_url():
    c = client_config()
    pending = {'state': secrets.token_urlsafe(32), 'verifier': secrets.token_urlsafe(64),
               'created_at': time.time()}
    save_private(HOME / 'calendar_oauth_pending.json', pending)
    challenge = base64.urlsafe_b64encode(hashlib.sha256(
        pending['verifier'].encode()).digest()).decode().rstrip('=')
    params = {'client_id': c['client_id'], 'redirect_uri': REDIRECT,
              'response_type': 'code', 'scope': ' '.join(SCOPES),
              'access_type': 'offline', 'prompt': 'consent',
              'state': pending['state'], 'code_challenge': challenge,
              'code_challenge_method': 'S256'}
    return {'auth_url': 'https://accounts.google.com/o/oauth2/v2/auth?' +
            urllib.parse.urlencode(params)}


def auth_code(redirect_url):
    p = HOME / 'calendar_oauth_pending.json'
    pending = json.loads(p.read_text())
    query = urllib.parse.parse_qs(urllib.parse.urlsplit(redirect_url).query)
    if time.time() - pending['created_at'] > 1800:
        raise ValueError('Authorization session expired; run auth-url again')
    if not secrets.compare_digest(query.get('state', [''])[0], pending['state']):
        raise ValueError('OAuth state mismatch; use the complete redirect URL')
    if not query.get('code'):
        raise ValueError('Authorization not granted')
    c = client_config()
    token = request_json(TOKEN_URL, method='POST', form=True, body={
        'client_id': c['client_id'], 'client_secret': c['client_secret'],
        'code': query['code'][0], 'code_verifier': pending['verifier'],
        'redirect_uri': REDIRECT, 'grant_type': 'authorization_code'})
    granted = set(token.get('scope', '').split())
    if not set(SCOPES).issubset(granted) or not token.get('refresh_token'):
        raise ValueError('Calendar permissions or offline access not granted; authorize again')
    token['expires_at'] = time.time() + token.get('expires_in', 3600)
    save_private(HOME / 'calendar_token.json', token)
    p.unlink()
    return {'authenticated': True, 'scopes': sorted(granted)}


def access_token():
    path = HOME / 'calendar_token.json'
    if not path.exists():
        raise ValueError('Calendar not authorized; run auth-url')
    token = json.loads(path.read_text())
    if token.get('expires_at', 0) < time.time() + 60:
        c = client_config()
        fresh = request_json(TOKEN_URL, method='POST', form=True, body={
            'client_id': c['client_id'], 'client_secret': c['client_secret'],
            'refresh_token': token['refresh_token'], 'grant_type': 'refresh_token'})
        token.update(fresh)
        token['expires_at'] = time.time() + fresh.get('expires_in', 3600)
        save_private(path, token)
    return token['access_token']


def api(path, *, method='GET', body=None, params=None, etag=None):
    headers = {'Authorization': 'Bearer ' + access_token()}
    if etag:
        headers['If-Match'] = etag
    url = API + path
    if params:
        url += '?' + urllib.parse.urlencode(params)
    return request_json(url, method=method, body=body, headers=headers)


def paged(path, params=None):
    params = dict(params or {})
    items = []
    while True:
        result = api(path, params=params)
        items.extend(result.get('items', []))
        if not result.get('nextPageToken'):
            return items
        params['pageToken'] = result['nextPageToken']


def event_path(calendar, event_id=None):
    path = '/calendars/' + urllib.parse.quote(calendar, safe='') + '/events'
    if event_id:
        path += '/' + urllib.parse.quote(event_id, safe='')
    return path


def timestamp(value):
    if 'T' not in value:
        raise ValueError('Use RFC3339 time with timezone, e.g. 2026-09-21T17:00:00+09:00')
    parsed = datetime.fromisoformat(value.replace('Z', '+00:00'))
    if parsed.tzinfo is None:
        raise ValueError('Timezone is required')
    return parsed


def event_body(summary=None, start=None, end=None, location=None, description=None):
    body = {k: v for k, v in [('summary', summary), ('location', location),
                             ('description', description)] if v is not None}
    if summary is not None and not summary.strip():
        raise ValueError('Summary cannot be empty')
    if (start is None) != (end is None):
        raise ValueError('Supply start and end together')
    if start is not None:
        if timestamp(end) <= timestamp(start):
            raise ValueError('End must be later than start')
        body.update(start={'dateTime': start}, end={'dateTime': end})
    if not body:
        raise ValueError('No changes supplied')
    return body


def create(calendar, event_id, **fields):
    # Caller supplies a reusable ID: network retries cannot create duplicates.
    if not re.fullmatch('[0-9a-v]{5,1024}', event_id):
        raise ValueError('Event ID must be 5–1024 base32hex characters; use uuid.uuid4().hex')
    if not fields.get('summary') or not fields.get('start') or not fields.get('end'):
        raise ValueError('Create requires summary, start and end')
    body = event_body(**fields)
    body['id'] = event_id
    return api(event_path(calendar), method='POST', body=body, params={'sendUpdates': 'none'})


def update(calendar, event_id, etag, **fields):
    if not etag:
        raise ValueError('Read the event first and supply its etag')
    body = event_body(**fields)
    old = api(event_path(calendar, event_id))
    if old.get('etag') != etag:
        raise ValueError('Event changed; read it again before applying changes')
    if old.get('recurrence'):
        raise ValueError('Select a single recurring instance; series updates are not supported')
    if old.get('status') == 'cancelled':
        raise ValueError('Cannot update a cancelled event')
    if 'start' in body and 'date' in old.get('start', {}):
        raise ValueError('All-day time conversion is not supported')
    return api(event_path(calendar, event_id), method='PATCH', body=body,
               params={'sendUpdates': 'none'}, etag=etag)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    sub = p.add_subparsers(dest='command', required=True)
    for name in ['now', 'auth-url', 'calendars']:
        sub.add_parser(name)
    sub.add_parser('auth-code').add_argument('--redirect-file', required=True,
        help='File containing the complete redirect URL (keeps code out of process arguments)')
    for name in ['list', 'get', 'create', 'update']:
        q = sub.add_parser(name)
        q.add_argument('--calendar', default='primary')
        if name in ['get', 'create', 'update']:
            q.add_argument('--event-id', required=True)
        if name == 'update':
            q.add_argument('--etag', required=True)
        if name in ['list', 'create', 'update']:
            q.add_argument('--start', required=name != 'update')
            q.add_argument('--end', required=name != 'update')
        if name == 'list':
            q.add_argument('--query')
        if name in ['create', 'update']:
            q.add_argument('--summary', required=name == 'create')
            q.add_argument('--location')
            q.add_argument('--description')
    a = p.parse_args()
    if a.command == 'now':
        result = {'now': datetime.now(ZoneInfo('Asia/Seoul')).isoformat()}
    elif a.command == 'auth-url':
        result = auth_url()
    elif a.command == 'auth-code':
        result = auth_code(Path(a.redirect_file).read_text().strip())
    elif a.command == 'calendars':
        result = paged('/users/me/calendarList')
    elif a.command == 'get':
        result = api(event_path(a.calendar, a.event_id))
    elif a.command == 'list':
        if timestamp(a.end) <= timestamp(a.start):
            raise ValueError('End must be later than start')
        params = {'timeMin': a.start, 'timeMax': a.end, 'singleEvents': 'true',
                  'orderBy': 'startTime', 'maxResults': 250, 'showDeleted': 'false'}
        if a.query:
            params['q'] = a.query
        result = paged(event_path(a.calendar), params)
    else:
        fields = {k: getattr(a, k) for k in ['summary', 'start', 'end', 'location', 'description']}
        if a.command == 'create':
            result = create(a.calendar, a.event_id, **fields)
        else:
            result = update(a.calendar, a.event_id, a.etag, **fields)
    print(json.dumps(result, ensure_ascii=False))


if __name__ == '__main__':
    try:
        main()
    except (ValueError, RuntimeError, OSError, KeyError) as e:
        print(json.dumps({'error': str(e)}, ensure_ascii=False), file=sys.stderr)
        sys.exit(1)
