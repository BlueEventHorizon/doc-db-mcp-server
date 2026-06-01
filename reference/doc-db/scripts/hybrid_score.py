#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Embedding / Lexical スコア統合。"""

from __future__ import annotations

from typing import Dict, List

# lex ヒット率（len(lex_items) / len(emb_items)）がこの値未満の場合、
# RRF ではなく emb スコア降順でフォールバックする。
# 日本語クエリで Lexical がほぼヒットしない場合に Embedding を優先するため。
EMB_FALLBACK_LEX_RATIO = 0.05

# hybrid recall(top-N) ≥ emb recall(top-N) を保証するための定数。
# emb 上位 K 件が RRF スコアに関わらず必ず fused の上位 K 件に含まれるよう昇格させる。
# クロスランゲージ同義語など lex=0 の文書が RRF で押し出される問題を防ぐ。
EMB_GUARANTEE_K = 5


def _to_rank_map(items: List[Dict], score_key: str) -> Dict[str, int]:
    ordered = sorted(items, key=lambda x: (-float(x.get(score_key, 0.0)), x["chunk_id"]))
    return {item["chunk_id"]: rank + 1 for rank, item in enumerate(ordered)}


def _to_score_map(items: List[Dict], score_key: str) -> Dict[str, float]:
    return {item["chunk_id"]: float(item.get(score_key, 0.0)) for item in items}


def rrf_fuse(emb_items: List[Dict], lex_items: List[Dict], k: int = 60) -> List[Dict]:
    # lex ヒット率が低い場合は emb only にフォールバック
    if emb_items and len(lex_items) / len(emb_items) < EMB_FALLBACK_LEX_RATIO:
        sorted_emb = sorted(emb_items, key=lambda x: (-x["emb_score"], x["chunk_id"]))
        return [{"chunk_id": r["chunk_id"], "score": r["emb_score"]} for r in sorted_emb]

    emb_rank = _to_rank_map(emb_items, "emb_score")
    lex_rank = _to_rank_map(lex_items, "lex_score")
    all_ids = set(emb_rank.keys()) | set(lex_rank.keys())
    fused: List[Dict] = []
    for chunk_id in all_ids:
        score = 0.0
        if chunk_id in emb_rank:
            score += 1.0 / (k + emb_rank[chunk_id])
        if chunk_id in lex_rank:
            score += 1.0 / (k + lex_rank[chunk_id])
        fused.append({"chunk_id": chunk_id, "score": score})
    fused.sort(key=lambda x: (-x["score"], x["chunk_id"]))

    # emb top-K 保証: emb 上位 K 件が fused 上位 K 件に含まれるよう昇格させる
    if EMB_GUARANTEE_K > 0 and emb_items:
        top_emb_ids = [
            item["chunk_id"]
            for item in sorted(emb_items, key=lambda x: (-x["emb_score"], x["chunk_id"]))[:EMB_GUARANTEE_K]
        ]
        top_emb_id_set = set(top_emb_ids)
        # 上位 K 件のうち保証対象外のアイテム（侵入者）を特定する
        intruders = [x for x in fused[:EMB_GUARANTEE_K] if x["chunk_id"] not in top_emb_id_set]
        if intruders:
            # 侵入者の最高スコアを超えるよう昇格させる
            promotion_threshold = max(x["score"] for x in intruders)
            guaranteed_not_in_top = top_emb_id_set - {x["chunk_id"] for x in fused[:EMB_GUARANTEE_K]}
            score_map = {x["chunk_id"]: x for x in fused}
            for rank_idx, cid in enumerate(top_emb_ids):
                if cid in guaranteed_not_in_top:
                    # emb ランク順を保持するため rank_idx に応じた微小オフセットを加える
                    score_map[cid]["score"] = promotion_threshold + (EMB_GUARANTEE_K - rank_idx) * 1e-9
            fused.sort(key=lambda x: (-x["score"], x["chunk_id"]))

    return fused


def linear_fuse(emb_items: List[Dict], lex_items: List[Dict], alpha: float = 0.7) -> List[Dict]:
    emb_scores = _to_score_map(emb_items, "emb_score")
    lex_scores = _to_score_map(lex_items, "lex_score")
    all_ids = set(emb_scores.keys()) | set(lex_scores.keys())
    merged: List[Dict] = []
    for chunk_id in all_ids:
        emb = emb_scores.get(chunk_id, 0.0)
        lex = lex_scores.get(chunk_id, 0.0)
        merged.append(
            {
                "chunk_id": chunk_id,
                "score": alpha * emb + (1.0 - alpha) * lex,
                "breakdown": {"emb": emb, "lex": lex},
            }
        )
    merged.sort(key=lambda x: (-x["score"], x["chunk_id"]))
    return merged


def combine_scores(
    emb_items: List[Dict],
    lex_items: List[Dict],
    method: str = "rrf",
    alpha: float = 0.7,
    k: int = 60,
) -> List[Dict]:
    if method == "linear":
        return linear_fuse(emb_items, lex_items, alpha=alpha)
    return rrf_fuse(emb_items, lex_items, k=k)
