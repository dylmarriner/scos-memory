import unittest
from scos_skill.sdk import SkillParameter, SkillDefinition, SkillManager


class SkillSDKTests(unittest.TestCase):
    def test_invoke(self):
        mgr = SkillManager()
        def_skill = SkillDefinition(
            skill_id="search",
            name="Search",
            description="Search skill",
            parameters=[SkillParameter("query", "str", "Search term", True)],
        )
        mgr.register(def_skill, lambda params: f"results for {params['query']}")
        res = mgr.invoke("search", {"query": "python"})
        self.assertEqual(res, "results for python")


if __name__ == "__main__":
    unittest.main()
