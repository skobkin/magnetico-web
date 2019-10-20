<?php

declare(strict_types=1);

namespace App\Doctrine\ORM\AST;

/**
 * Implementation of PostgreSql ILIKE().
 *
 * For usage example @see https://github.com/martin-georgiev/postgresql-for-doctrine/blob/master/docs/USE-CASES-AND-EXAMPLES.md
 *
 * @see https://www.postgresql.org/docs/9.3/functions-matching.html
 *
 * @author llaakkkk <lenakirichokv@gmail.com>
 * @see https://github.com/martin-georgiev/postgresql-for-doctrine
 */
class Ilike extends BaseFunction
{
    protected function customiseFunction(): void
    {
        $this->setFunctionPrototype('%s ilike %s');
        $this->addNodeMapping('StringPrimary');
        $this->addNodeMapping('StringPrimary');
    }
}
