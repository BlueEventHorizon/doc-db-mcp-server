#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Lexical 検索（BM25 スコアリング + ID 完全一致ブースト）。

正規化済みトークンの substring マッチングで TF を計算し、
IDF は全 chunk を対象として Robertson IDF 式で算出する。
クロスランゲージ同義語マッチングは Embedding 検索が担う。
"""

from __future__ import annotations

import math
import re
import unicodedata
from typing import Dict, List

ID_RE = re.compile(r"[A-Z]+-\d+")
# [A-Za-z]+-\d+ : 英数字 ID (例: FNC-001)
# [A-Za-z0-9_]+ : ASCII 英数字・アンダースコア
# [^\W\d_A-Za-z]+: CJK 等の非 ASCII・非数字 Unicode 単語文字
# \d+            : 数字（CJK との境界で分割するため独立させる）
TOKEN_RE = re.compile(r"[A-Za-z]+-\d+|[A-Za-z0-9_]+|[^\W\d_A-Za-z]+|\d+", re.UNICODE)

BM25_K1 = 1.5  # TF 飽和係数（標準パラメータ、ゴールデンセット評価で検証）
BM25_B = 0.75  # 文書長正規化係数（同上）


def normalize_text(text: str) -> str:
    return unicodedata.normalize("NFKC", text).lower()


def tokenize(text: str) -> List[str]:
    return [t for t in TOKEN_RE.findall(normalize_text(text)) if t]


def score_chunks(query: str, chunks: List[Dict]) -> List[Dict]:
    """BM25 スコアリングによる語彙一致検索。"""
    query_norm = normalize_text(query)
    query_tokens = [t for t in tokenize(query_norm) if t]
    id_tokens = [t.lower() for t in ID_RE.findall(unicodedata.normalize("NFKC", query).upper())]

    if not chunks:
        return []

    # 全 chunk の正規化済み body を事前計算
    normalized_bodies = [normalize_text(c.get("body", "")) for c in chunks]
    N = len(chunks)

    # Robertson IDF（substring マッチングベースの文書頻度）
    unique_tokens = set(query_tokens)
    idf: Dict[str, float] = {}
    for token in unique_tokens:
        df = sum(1 for b in normalized_bodies if token in b)
        idf[token] = math.log((N - df + 0.5) / (df + 0.5) + 1)

    # 文書長（文字数）の平均
    char_counts = [len(b) for b in normalized_bodies]
    avgdl = sum(char_counts) / N if N > 0 else 1.0

    results: List[Dict] = []

    for i, (chunk, body) in enumerate(zip(chunks, normalized_bodies)):
        dl = char_counts[i]
        score = 0.0

        # BM25 スコアリング
        for token in query_tokens:
            tf = body.count(token)
            if tf == 0:
                continue
            token_idf = idf.get(token, 0.0)
            tf_norm = (tf * (BM25_K1 + 1)) / (tf + BM25_K1 * (1 - BM25_B + BM25_B * dl / avgdl))
            score += token_idf * tf_norm

        # ID 完全一致ボーナス
        for id_token in id_tokens:
            if id_token in body:
                score += 10.0

        # クエリ全体の完全一致ボーナス
        if query_norm.strip() and query_norm in body:
            score += 2.0

        if score > 0:
            results.append(
                {
                    "chunk_id": chunk["chunk_id"],
                    "path": chunk.get("path", ""),
                    "heading_path": chunk.get("heading_path", []),
                    "lex_score": float(score),
                    "body": chunk.get("body", ""),
                }
            )

    results.sort(key=lambda x: (-x["lex_score"], x["chunk_id"]))
    return results
