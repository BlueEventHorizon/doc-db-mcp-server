use anyhow::{Result, ensure};

pub struct Chunk {
    pub heading_path: String,
    pub text: String,
    pub embed_text: String,
}

pub fn split(content: &str, max_chars: usize) -> Result<Vec<Chunk>> {
    ensure!(max_chars > 0, "チャンクの最大文字数が不正です");
    let mut chunks = Vec::new();
    let mut lines = Vec::new();
    let mut headings = Vec::new();
    let mut previous_prose = String::new();
    for line in content.lines() {
        ensure!(line.len() < 64 * 1024, "文書に長すぎる行があります");
        if let Some((level, title)) = heading(line) {
            append_section(
                &mut chunks,
                &lines.join("\n"),
                &headings,
                max_chars,
                &mut previous_prose,
            );
            lines.clear();
            headings.retain(|(existing, _)| *existing < level);
            headings.push((level, title));
        }
        lines.push(line);
    }
    append_section(
        &mut chunks,
        &lines.join("\n"),
        &headings,
        max_chars,
        &mut previous_prose,
    );
    if chunks.is_empty() {
        chunks.push(Chunk {
            heading_path: String::new(),
            text: content.trim_end_matches('\n').to_owned(),
            embed_text: content.trim_end_matches('\n').to_owned(),
        });
    }
    Ok(chunks)
}

fn heading(line: &str) -> Option<(usize, &str)> {
    let rest = line.trim_start_matches('#');
    let level = line.len() - rest.len();
    if (1..=6).contains(&level) && (rest.is_empty() || rest.starts_with([' ', '\t'])) {
        Some((level, rest.trim()))
    } else {
        None
    }
}

fn append_section(
    chunks: &mut Vec<Chunk>,
    text: &str,
    headings: &[(usize, &str)],
    max_chars: usize,
    previous_prose: &mut String,
) {
    let text = text.trim_end_matches('\n');
    if text.is_empty() {
        return;
    }
    let heading_path = headings
        .iter()
        .map(|(_, title)| *title)
        .collect::<Vec<_>>()
        .join(" > ");
    let count = text.chars().count();
    let part_size = count.div_ceil(count.div_ceil(max_chars));
    let boundaries = text
        .char_indices()
        .step_by(part_size)
        .map(|(index, _)| index);
    let ends = boundaries
        .clone()
        .skip(1)
        .chain(std::iter::once(text.len()));
    for (start, end) in boundaries.zip(ends) {
        let part = &text[start..end];
        let prose = part
            .split('\n')
            .filter(|line| !line.starts_with('#'))
            .collect::<Vec<_>>()
            .join("\n");
        let mut prose = prose.trim();
        if prose.chars().count() >= 50 {
            previous_prose.clear();
            previous_prose.push_str(prose);
        } else if !previous_prose.is_empty() {
            prose = previous_prose;
        }
        let embed_text = match (heading_path.is_empty(), prose.is_empty()) {
            (false, false) => format!("{heading_path}\n\n{prose}"),
            (false, true) => heading_path.clone(),
            (true, false) => prose.to_owned(),
            (true, true) => part.to_owned(),
        };
        chunks.push(Chunk {
            heading_path: heading_path.clone(),
            text: part.to_owned(),
            embed_text,
        });
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn unicode_split_and_heading_context() {
        let chunks = split("# 見出し\n## 詳細\nあいうえお", 6).unwrap();
        assert_eq!(chunks[0].heading_path, "見出し");
        assert_eq!(chunks[1].heading_path, "見出し > 詳細");
        assert!(chunks.iter().all(|chunk| chunk.text.chars().count() <= 6));
        assert_eq!(
            chunks[1..]
                .iter()
                .map(|chunk| chunk.text.as_str())
                .collect::<String>(),
            "## 詳細\nあいうえお"
        );
    }
}
