import unittest
from scos_agent.client import AgentAdapterClient


class AgentClientTests(unittest.TestCase):
    def test_render_envelope(self):
        client = AgentAdapterClient("agent-1")
        env = client.render_envelope("sess-1", "tenant-1")
        self.assertEqual(env.agent_id, "agent-1")
        self.assertEqual(env.session_id, "sess-1")


if __name__ == "__main__":
    unittest.main()
