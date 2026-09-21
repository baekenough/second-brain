import importlib.util
import json
from pathlib import Path
import tempfile
import time
import unittest
from unittest.mock import patch
import urllib.error

spec = importlib.util.spec_from_file_location('calendar_client', Path(__file__).with_name('calendar_client.py'))
c = importlib.util.module_from_spec(spec)
spec.loader.exec_module(c)


class CalendarClientTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name)
        self.patch = patch.object(c, 'HOME', self.home)
        self.patch.start()
        self.addCleanup(self.patch.stop)
        c.save_private(self.home / 'calendar_client_secret.json', {'installed': {'client_id': 'id', 'client_secret': 'private'}})
        c.save_private(self.home / 'calendar_oauth_pending.json', {'state': 'expected', 'verifier': 'verifier', 'created_at': time.time()})

    def test_oauth_state_rejected_before_exchange(self):
        with patch.object(c, 'request_json') as request:
            with self.assertRaisesRegex(ValueError, 'state mismatch'):
                c.auth_code(c.REDIRECT + '?state=wrong&code=secret')
            request.assert_not_called()

    def test_missing_scopes_never_saves_token(self):
        with patch.object(c, 'request_json', return_value={'scope': c.SCOPES[0], 'refresh_token': 'secret'}):
            with self.assertRaisesRegex(ValueError, 'permissions'):
                c.auth_code(c.REDIRECT + '?state=expected&code=secret')
        self.assertFalse((self.home / 'calendar_token.json').exists())

    def test_valid_oauth_private_token_and_consumed_state(self):
        with patch.object(c, 'request_json', return_value={'scope': ' '.join(c.SCOPES), 'refresh_token': 'secret'}):
            self.assertTrue(c.auth_code(c.REDIRECT + '?state=expected&code=secret')['authenticated'])
        self.assertEqual((self.home / 'calendar_token.json').stat().st_mode & 0o777, 0o600)
        self.assertFalse((self.home / 'calendar_oauth_pending.json').exists())

    def test_patch_only_changed_fields_with_etag_no_notifications(self):
        old = {'etag': 'version', 'attendees': [{'email': 'guest@example.com'}], 'reminders': {'useDefault': True}, 'start': {'dateTime': '2026-09-21T17:00:00+09:00'}}
        with patch.object(c, 'api', side_effect=[old, {'id': 'event'}]) as api:
            c.update('primary', 'event', 'version', location='New room')
        self.assertEqual(api.call_args.kwargs, {'method': 'PATCH', 'body': {'location': 'New room'}, 'params': {'sendUpdates': 'none'}, 'etag': 'version'})
        self.assertIn('attendees', old)

    def test_stale_etag_never_patches(self):
        with patch.object(c, 'api', return_value={'etag': 'new'}) as api:
            with self.assertRaisesRegex(ValueError, 'changed'):
                c.update('primary', 'event', 'old', summary='Interview')
        self.assertEqual(api.call_count, 1)

    def test_timezone_required(self):
        with self.assertRaisesRegex(ValueError, 'Timezone'):
            c.event_body(start='2026-09-21T17:00:00', end='2026-09-21T18:00:00')

    def test_repeated_create_uses_same_id(self):
        with patch.object(c, 'api', return_value={}) as api:
            for _ in range(2):
                c.create('primary', 'abcde12345', summary='Interview', start='2026-09-21T17:00:00+09:00', end='2026-09-21T18:00:00+09:00')
            self.assertEqual(api.call_args_list[0], api.call_args_list[1])
            self.assertEqual(api.call_args.kwargs['params'], {'sendUpdates': 'none'})

    def test_http_error_body_never_exposed(self):
        error = urllib.error.HTTPError('https://google.invalid', 403, 'access_token=secret', {}, None)
        with patch.object(c.urllib.request, 'urlopen', side_effect=error):
            with self.assertRaises(RuntimeError) as raised:
                c.request_json('https://google.invalid')
        self.assertNotIn('secret', str(raised.exception))

    def test_pagination(self):
        with patch.object(c, 'api', side_effect=[{'items': [1], 'nextPageToken': 'next'}, {'items': [2]}]) as api:
            self.assertEqual(c.paged('/events'), [1, 2])
            self.assertEqual(api.call_args.kwargs['params']['pageToken'], 'next')


if __name__ == '__main__':
    unittest.main()
