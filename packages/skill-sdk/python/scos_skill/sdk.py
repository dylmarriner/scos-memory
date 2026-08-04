"""Python skill SDK package (§22.5)."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Callable


@dataclass
class SkillParameter:
    name: str
    param_type: str
    description: str
    required: bool = True


@dataclass
class SkillDefinition:
    skill_id: str
    name: str
    description: str
    parameters: list[SkillParameter] = field(default_factory=list)


class SkillManager:
    def __init__(self):
        self._skills: dict[str, tuple[SkillDefinition, Callable[[dict[str, str]], str]]] = {}

    def register(self, definition: SkillDefinition, executor: Callable[[dict[str, str]], str]) -> None:
        self._skills[definition.skill_id] = (definition, executor)

    def invoke(self, skill_id: str, params: dict[str, str]) -> str:
        if skill_id not in self._skills:
            raise KeyError(f"Skill {skill_id} not registered")
        definition, executor = self._skills[skill_id]
        for p in definition.parameters:
            if p.required and p.name not in params:
                raise ValueError(f"Missing required parameter: {p.name}")
        return executor(params)
