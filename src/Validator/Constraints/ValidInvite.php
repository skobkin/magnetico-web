<?php
declare(strict_types=1);

namespace App\Validator\Constraints;

use Symfony\Component\Validator\Constraint;

#[\Attribute(\Attribute::TARGET_PROPERTY | \Attribute::TARGET_METHOD)]
class ValidInvite extends Constraint
{
    public $notFoundMessage = 'Invite {{ code }} not found.';
    public $usedMessage = 'Invite {{ code }} is used.';

    public function validatedBy(): string
    {
        return static::class.'Validator';
    }
}
