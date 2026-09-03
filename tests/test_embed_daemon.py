import json
import math
import socket
import unittest

import embed_daemon


class FakeVector:
    def __init__(self, values):
        self.values = values

    def tolist(self):
        return self.values


class FakeModel:
    def __init__(self):
        self.calls = []
        self.query_vector = [1.0] + [0.0] * 767
        self.passage_vector = [0.0, 1.0] + [0.0] * 766

    def query_embed(self, texts):
        self.calls.append(("query", texts))
        return iter([FakeVector(self.query_vector)])

    def passage_embed(self, texts):
        self.calls.append(("passage", texts))
        return iter([FakeVector(self.passage_vector)])


def cosine_distance(left, right):
    dot = sum(a * b for a, b in zip(left, right))
    left_norm = math.sqrt(sum(value * value for value in left))
    right_norm = math.sqrt(sum(value * value for value in right))
    return 1.0 - dot / (left_norm * right_norm)


class EmbedDaemonTests(unittest.TestCase):
    def test_legacy_plain_text_remains_query_mode(self):
        self.assertEqual(embed_daemon.parse_request("legacy query"), ("query", "legacy query"))

    def test_explicit_passage_mode_uses_passage_embed(self):
        model = FakeModel()
        mode, text = embed_daemon.parse_request(
            json.dumps({"mode": "passage", "text": "future document"})
        )
        vector = embed_daemon.embed_text(model, text, mode)
        self.assertEqual(model.calls, [("passage", ["future document"])])
        self.assertEqual(vector, model.passage_vector)

    def test_same_passage_content_has_zero_distance_to_own_stored_vector(self):
        model = FakeModel()
        stored_document_vector = model.passage_vector
        candidate_vector = embed_daemon.embed_text(model, "same content", "passage")
        self.assertAlmostEqual(cosine_distance(candidate_vector, stored_document_vector), 0.0)
        query_vector = embed_daemon.embed_text(model, "same content", "query")
        self.assertGreater(cosine_distance(query_vector, stored_document_vector), 0.9)

    def test_socket_response_for_passage_request_is_legacy_vector_shape(self):
        server, client = socket.socketpair()
        try:
            client.sendall(b'{"mode":"passage","text":"future document"}\n')
            embed_daemon.handle_client(server, FakeModel())
            response = client.recv(65_536)
        finally:
            client.close()
        vector = json.loads(response)
        self.assertEqual(len(vector), embed_daemon.EMBEDDING_DIMENSION)
        self.assertEqual(vector[1], 1.0)

    def test_invalid_protocol_returns_null_without_raising(self):
        server, client = socket.socketpair()
        try:
            client.sendall(b'{"mode":"document","text":"x"}\n')
            embed_daemon.handle_client(server, FakeModel())
            response = client.recv(64)
        finally:
            client.close()
        self.assertEqual(response, b"null\n")


if __name__ == "__main__":
    unittest.main()
