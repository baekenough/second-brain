"use client";

type Sheet = { name: string; rows: string[][] };

function parseTsv(content: string): Sheet[] {
  const sheets: Sheet[] = [];
  let current: Sheet | null = null;
  for (const line of content.split("\n")) {
    if (line.startsWith("##SHEET ")) {
      if (current && current.rows.length) sheets.push(current);
      current = { name: line.slice(8).trim(), rows: [] };
      continue;
    }
    if (!current) continue;
    if (line.trim() === "") continue;
    current.rows.push(line.split("\t"));
  }
  if (current && current.rows.length) sheets.push(current);
  return sheets;
}

export default function XlsxTable({ content }: { content: string }) {
  const sheets = parseTsv(content);
  if (sheets.length === 0) {
    return (
      <p className="text-sm text-gray-500 dark:text-gray-400">
        이 파일에는 표시할 시트가 없습니다.
      </p>
    );
  }
  return (
    <div className="space-y-6">
      {sheets.map((s) => {
        const header = s.rows[0] ?? [];
        const body = s.rows.slice(1);
        return (
          <section key={s.name}>
            <h3 className="text-xs font-semibold text-gray-500 dark:text-gray-400 mb-2">
              {s.name}{" "}
              <span className="text-gray-400 dark:text-gray-600">
                ({body.length}행)
              </span>
            </h3>
            <div className="overflow-x-auto border border-gray-200 dark:border-gray-800 rounded">
              <table className="min-w-full text-xs">
                <thead className="bg-gray-50 dark:bg-gray-900">
                  <tr>
                    {header.map((cell, i) => (
                      <th
                        key={i}
                        className="px-3 py-2 text-left font-medium text-gray-700 dark:text-gray-300 whitespace-nowrap"
                      >
                        {cell}
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {body.slice(0, 200).map((row, r) => (
                    <tr
                      key={r}
                      className="border-t border-gray-100 dark:border-gray-800"
                    >
                      {row.map((cell, c) => (
                        <td
                          key={c}
                          className="px-3 py-1.5 text-gray-600 dark:text-gray-400 whitespace-nowrap"
                        >
                          {cell}
                        </td>
                      ))}
                    </tr>
                  ))}
                </tbody>
              </table>
              {body.length > 200 && (
                <p className="px-3 py-2 text-xs text-gray-400 dark:text-gray-500 border-t border-gray-100 dark:border-gray-800">
                  처음 200행만 표시합니다. 전체 {body.length}행.
                </p>
              )}
            </div>
          </section>
        );
      })}
    </div>
  );
}
