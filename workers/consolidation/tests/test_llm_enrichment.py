"""Tests for the optional LLM enrichment pass."""

import json
import os
import sys
import unittest
import urllib.error
from pathlib import Path
from unittest.mock import patch
from uuid import uuid4

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from scos_consolidation.models import EvidenceReference, MemoryCandidate, MemoryClass, SensitivityClass
from scos_consolidation.llm_enrichment import enrich_candidates, _deepseek_config


def _candidate(**overrides) -> MemoryCandidate:
    defaults = dict(
        candidate_id=uuid4(),
        tenant_id=uuid4(),
        namespace_id=uuid4(),
        source_event_id=uuid4(),
        memory_class=MemoryClass.SEMANTIC,
        title="Original title",
        content="[user]\nDoes X support Y?\n\n[assistant]\nYes, X supports Y via Z.",
        authority_level=70,
        confidence_score=0.75,
        sensitivity_class=SensitivityClass.INTERNAL,
        evidence=[EvidenceReference(kind="event", reference_id="evt-1")],
    )
    defaults.update(overrides)
    return MemoryCandidate(**defaults)


class DeepseekConfigTests(unittest.TestCase):
    def test_disabled_without_api_key(self):
        with patch.dict(os.environ, {}, clear=False):
            os.environ.pop("DEEPSEEK_API_KEY", None)
            self.assertIsNone(_deepseek_config())

    def test_enabled_with_defaults(self):
        with patch.dict(os.environ, {"DEEPSEEK_API_KEY": "sk-test"}, clear=False):
            os.environ.pop("DEEPSEEK_BASE_URL", None)
            os.environ.pop("DEEPSEEK_MODEL", None)
            api_key, base_url, model = _deepseek_config()
            self.assertEqual(api_key, "sk-test")
            self.assertEqual(base_url, "https://api.deepseek.com/v1")
            self.assertEqual(model, "deepseek-chat")


class EnrichCandidatesTests(unittest.TestCase):
    def test_passthrough_when_disabled(self):
        with patch.dict(os.environ, {}, clear=False):
            os.environ.pop("DEEPSEEK_API_KEY", None)
            candidates = [_candidate()]
            result = enrich_candidates(candidates)
            self.assertIs(result, candidates)
            self.assertEqual(result[0].content, candidates[0].content)

    def test_rewrites_title_and_content_on_success(self):
        candidate = _candidate()
        fake_response = json.dumps({
            "choices": [{"message": {"content": json.dumps({
                "title": "X supports Y",
                "content": "X supports Y via Z.",
            })}}]
        }).encode("utf-8")

        class FakeResp:
            def __enter__(self):
                return self
            def __exit__(self, *a):
                return False
            def read(self):
                return fake_response

        with patch.dict(os.environ, {"DEEPSEEK_API_KEY": "sk-test"}, clear=False):
            with patch("scos_consolidation.llm_enrichment.urllib.request.urlopen", return_value=FakeResp()):
                result = enrich_candidates([candidate])
        self.assertEqual(len(result), 1)
        self.assertEqual(result[0].title, "X supports Y")
        self.assertEqual(result[0].content, "X supports Y via Z.")
        # Governance-critical fields must be untouched by enrichment.
        self.assertEqual(result[0].memory_class, candidate.memory_class)
        self.assertEqual(result[0].authority_level, candidate.authority_level)
        self.assertEqual(result[0].confidence_score, candidate.confidence_score)
        self.assertEqual(result[0].sensitivity_class, candidate.sensitivity_class)

    def test_falls_back_to_original_on_network_error(self):
        candidate = _candidate()
        with patch.dict(os.environ, {"DEEPSEEK_API_KEY": "sk-test"}, clear=False):
            with patch("scos_consolidation.llm_enrichment.urllib.request.urlopen",
                       side_effect=urllib.error.URLError("boom")):
                result = enrich_candidates([candidate])
        self.assertEqual(len(result), 1)
        self.assertEqual(result[0].content, candidate.content)
        self.assertEqual(result[0].title, candidate.title)

    def test_falls_back_to_original_on_empty_model_output(self):
        candidate = _candidate()
        fake_response = json.dumps({
            "choices": [{"message": {"content": json.dumps({"title": "", "content": ""})}}]
        }).encode("utf-8")

        class FakeResp:
            def __enter__(self):
                return self
            def __exit__(self, *a):
                return False
            def read(self):
                return fake_response

        with patch.dict(os.environ, {"DEEPSEEK_API_KEY": "sk-test"}, clear=False):
            with patch("scos_consolidation.llm_enrichment.urllib.request.urlopen", return_value=FakeResp()):
                result = enrich_candidates([candidate])
        self.assertEqual(result[0].content, candidate.content)


if __name__ == "__main__":
    unittest.main()
