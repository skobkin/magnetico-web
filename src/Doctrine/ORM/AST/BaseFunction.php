<?php
declare(strict_types=1);

namespace App\Doctrine\ORM\AST;

use Doctrine\ORM\Query\{AST\Functions\FunctionNode, AST\Node, Lexer, Parser, SqlWalker};

/**
 * @since 0.1
 *
 * @author Martin Georgiev <martin.georgiev@gmail.com>
 * @see https://github.com/martin-georgiev/postgresql-for-doctrine
 */
abstract class BaseFunction extends FunctionNode
{
    protected string $functionPrototype;

    /** @var string[] */
    protected array $nodesMapping = [];

    /** @var Node[] */
    protected array $nodes = [];

    abstract protected function customiseFunction(): void;

    protected function setFunctionPrototype(string $functionPrototype): void
    {
        $this->functionPrototype = $functionPrototype;
    }

    protected function addNodeMapping(string $parserMethod): void
    {
        $this->nodesMapping[] = $parserMethod;
    }

    public function parse(Parser $parser): void
    {
        $this->customiseFunction();

        $parser->match(Lexer::T_IDENTIFIER);
        $parser->match(Lexer::T_OPEN_PARENTHESIS);
        $this->feedParserWithNodes($parser);
        $parser->match(Lexer::T_CLOSE_PARENTHESIS);
    }

    /**
     * Feeds given parser with previously set nodes.
     */
    protected function feedParserWithNodes(Parser $parser): void
    {
        $nodesMappingCount = \count($this->nodesMapping);
        $lastNode = $nodesMappingCount - 1;
        for ($i = 0; $i < $nodesMappingCount; $i++) {
            $parserMethod = $this->nodesMapping[$i];
            $this->nodes[$i] = $parser->{$parserMethod}();
            if ($i < $lastNode) {
                $parser->match(Lexer::T_COMMA);
            }
        }
    }

    public function getSql(SqlWalker $sqlWalker): string
    {
        $dispatched = [];
        foreach ($this->nodes as $node) {
            $dispatched[] = $node->dispatch($sqlWalker);
        }

        return \vsprintf($this->functionPrototype, $dispatched);
    }
}
