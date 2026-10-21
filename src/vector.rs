use scos_common::MemoryObject;
use sha2::{Digest, Sha256};
use std::collections::HashSet;

/// Vector embedding computation using deterministic hashing.
pub struct VectorEmbedding;

impl VectorEmbedding {
    /// Compute semantic hash for a memory object using content pointer-based hashing.
    pub fn semantic_hash(obj: &MemoryObject) -> String {
        let mut hasher = Sha256::new();
        hasher.update(obj.content_pointer.as_bytes());
        hasher.update(obj.semantic_hash.as_bytes());
        format!("{:x}", hasher.finalize())
    }

    /// Compute embedding hash for a memory object using type and content pointer.
    pub fn embedding_hash(obj: &MemoryObject) -> String {
        let mut hasher = Sha256::new();
        hasher.update(format!("{:?}", obj.memory_type).as_bytes());
        hasher.update(obj.content_pointer.as_bytes());
        format!("{:x}", hasher.finalize())
    }

    /// Compute similarity between two memory objects using Jaccard similarity on semantic hashes.
    pub fn similarity(a: &MemoryObject, b: &MemoryObject) -> f64 {
        let hash_a: HashSet<char> = a.semantic_hash.chars().collect();
        let hash_b: HashSet<char> = b.semantic_hash.chars().collect();

        if hash_a.is_empty() && hash_b.is_empty() {
            return 1.0;
        }

        let intersection = hash_a.intersection(&hash_b).count();
        let union = hash_a.union(&hash_b).count();

        if union == 0 {
            0.0
        } else {
            intersection as f64 / union as f64
        }
    }
}
