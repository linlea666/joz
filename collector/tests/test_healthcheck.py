import unittest
from healthcheck import healthy

class HealthTests(unittest.TestCase):
    def test_ipc_is_distinct_from_discord_login(self):
        self.assertTrue(healthy({'ipc_configured': True, 'checked_at': 100, 'discord_state':'disabled'}, 105))
        self.assertFalse(healthy({'ipc_configured': False, 'checked_at': 100, 'discord_state':'connected'}, 105))
        self.assertFalse(healthy({'ipc_configured': True, 'checked_at': 100}, 121))
        self.assertFalse(healthy({'ipc_configured': True, 'checked_at': 100}, 99))
