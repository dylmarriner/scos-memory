import unittest
from datetime import datetime, timezone
from scos_routing.contracts import IngressEvent, IntentClassification, RouteDecision


class RoutingContractsTests(unittest.TestCase):
    def test_event_hash(self):
        now = datetime.now(timezone.utc)
        evt = IngressEvent(
            event_id="e1",
            session_id="s1",
            tenant_id="t1",
            actor_id="u1",
            agent_id="a1",
            content="test content",
            occurred_at=now,
        )
        h1 = evt.compute_event_hash()
        h2 = evt.compute_event_hash()
        self.assertEqual(h1, h2)
        self.assertTrue(len(h1) > 0)


if __name__ == "__main__":
    unittest.main()
