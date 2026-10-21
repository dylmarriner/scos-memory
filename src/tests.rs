#[cfg(test)]
mod tests {
    use super::*;
    use crate::retrieval::MemoryRetrieval;
    use crate::vector::VectorEmbedding;
    use crate::MemoryStore;
    use scos_common::{MemoryObject, MemoryProvenance, MemoryType, NodeId};

    #[test]
    fn test_memory_store_basic_operations() {
        let store = MemoryStore::open(std::path::Path::new("/tmp/test-memory")).unwrap();

        let memory = MemoryObject {
            id: "test-1".to_string(),
            memory_type: MemoryType::Task,
            semantic_hash: "hash1".to_string(),
            embedding_hash: None,
            owner_node: NodeId("node1".to_string()),
            content_pointer: "content1".to_string(),
            trust_score: 1.0,
            provenance: MemoryProvenance {
                origin_node_id: NodeId("node1".to_string()),
                transaction_id: None,
                source_tool_id: None,
                prev_version_id: None,
            },
            graph_links: vec![],
            created_at_ms: 0,
            updated_at_ms: 0,
        };

        store.insert(&memory);

        assert!(store.get("test-1").is_some());
        assert!(store.get("nonexistent").is_none());
    }

    #[test]
    fn test_memory_store_all() {
        let store = MemoryStore::open(std::path::Path::new("/tmp/test-memory")).unwrap();

        let memory1 = MemoryObject {
            id: "test-1".to_string(),
            memory_type: MemoryType::Task,
            semantic_hash: "hash1".to_string(),
            embedding_hash: None,
            owner_node: NodeId("node1".to_string()),
            content_pointer: "content1".to_string(),
            trust_score: 1.0,
            provenance: MemoryProvenance {
                origin_node_id: NodeId("node1".to_string()),
                transaction_id: None,
                source_tool_id: None,
                prev_version_id: None,
            },
            graph_links: vec![],
            created_at_ms: 0,
            updated_at_ms: 0,
        };

        let memory2 = MemoryObject {
            id: "test-2".to_string(),
            memory_type: MemoryType::AgentReasoning,
            semantic_hash: "hash2".to_string(),
            embedding_hash: None,
            owner_node: NodeId("node2".to_string()),
            content_pointer: "content2".to_string(),
            trust_score: 0.8,
            provenance: MemoryProvenance {
                origin_node_id: NodeId("node2".to_string()),
                transaction_id: None,
                source_tool_id: None,
                prev_version_id: None,
            },
            graph_links: vec![],
            created_at_ms: 0,
            updated_at_ms: 0,
        };

        store.insert(&memory1);
        store.insert(&memory2);

        let all = store.all();
        assert_eq!(all.len(), 2);
    }

    #[test]
    fn test_vector_embedding_semantic_hash() {
        let memory = MemoryObject {
            id: "test".to_string(),
            memory_type: MemoryType::Task,
            semantic_hash: "semantic".to_string(),
            embedding_hash: None,
            owner_node: NodeId("node1".to_string()),
            content_pointer: "content".to_string(),
            trust_score: 1.0,
            provenance: MemoryProvenance {
                origin_node_id: NodeId("node1".to_string()),
                transaction_id: None,
                source_tool_id: None,
                prev_version_id: None,
            },
            graph_links: vec![],
            created_at_ms: 0,
            updated_at_ms: 0,
        };

        let hash = VectorEmbedding::semantic_hash(&memory);
        assert!(!hash.is_empty());
        assert_eq!(hash.len(), 64); // SHA256 produces 64 hex characters
    }

    #[test]
    fn test_vector_embedding_similarity() {
        let memory1 = MemoryObject {
            id: "test-1".to_string(),
            memory_type: MemoryType::Task,
            semantic_hash: "similar content test".to_string(),
            embedding_hash: None,
            owner_node: NodeId("node1".to_string()),
            content_pointer: "content1".to_string(),
            trust_score: 1.0,
            provenance: MemoryProvenance {
                origin_node_id: NodeId("node1".to_string()),
                transaction_id: None,
                source_tool_id: None,
                prev_version_id: None,
            },
            graph_links: vec![],
            created_at_ms: 0,
            updated_at_ms: 0,
        };

        let memory2 = MemoryObject {
            id: "test-2".to_string(),
            memory_type: MemoryType::Task,
            semantic_hash: "similar content test".to_string(),
            embedding_hash: None,
            owner_node: NodeId("node2".to_string()),
            content_pointer: "content2".to_string(),
            trust_score: 1.0,
            provenance: MemoryProvenance {
                origin_node_id: NodeId("node2".to_string()),
                transaction_id: None,
                source_tool_id: None,
                prev_version_id: None,
            },
            graph_links: vec![],
            created_at_ms: 0,
            updated_at_ms: 0,
        };

        let similarity = VectorEmbedding::similarity(&memory1, &memory2);
        assert_eq!(similarity, 1.0); // Identical hashes should have similarity 1.0
    }

    #[test]
    fn test_vector_embedding_different_similarity() {
        let memory1 = MemoryObject {
            id: "test-1".to_string(),
            memory_type: MemoryType::Task,
            semantic_hash: "completely different".to_string(),
            embedding_hash: None,
            owner_node: NodeId("node1".to_string()),
            content_pointer: "content1".to_string(),
            trust_score: 1.0,
            provenance: MemoryProvenance {
                origin_node_id: NodeId("node1".to_string()),
                transaction_id: None,
                source_tool_id: None,
                prev_version_id: None,
            },
            graph_links: vec![],
            created_at_ms: 0,
            updated_at_ms: 0,
        };

        let memory2 = MemoryObject {
            id: "test-2".to_string(),
            memory_type: MemoryType::Task,
            semantic_hash: "another different thing".to_string(),
            embedding_hash: None,
            owner_node: NodeId("node2".to_string()),
            content_pointer: "content2".to_string(),
            trust_score: 1.0,
            provenance: MemoryProvenance {
                origin_node_id: NodeId("node2".to_string()),
                transaction_id: None,
                source_tool_id: None,
                prev_version_id: None,
            },
            graph_links: vec![],
            created_at_ms: 0,
            updated_at_ms: 0,
        };

        let similarity = VectorEmbedding::similarity(&memory1, &memory2);
        assert!(similarity < 1.0);
        assert!(similarity >= 0.0);
    }

    #[test]
    fn test_memory_retrieval_search() {
        let store = MemoryStore::open(std::path::Path::new("/tmp/test-memory")).unwrap();
        let retrieval = MemoryRetrieval::new(store);

        let memory = MemoryObject {
            id: "test-1".to_string(),
            memory_type: MemoryType::Task,
            semantic_hash: "test query content".to_string(),
            embedding_hash: None,
            owner_node: NodeId("node1".to_string()),
            content_pointer: "test query content".to_string(),
            trust_score: 1.0,
            provenance: MemoryProvenance {
                origin_node_id: NodeId("node1".to_string()),
                transaction_id: None,
                source_tool_id: None,
                prev_version_id: None,
            },
            graph_links: vec![],
            created_at_ms: 0,
            updated_at_ms: 0,
        };

        // Insert through the store directly since retrieval.store is private
        // We need to create a new store reference for the test
        let store = MemoryStore::open(std::path::Path::new("/tmp/test-memory")).unwrap();
        store.insert(&memory);
        let retrieval = MemoryRetrieval::new(store);

        let results = retrieval.search("test query", 10).unwrap();
        assert!(!results.is_empty());
    }
}
