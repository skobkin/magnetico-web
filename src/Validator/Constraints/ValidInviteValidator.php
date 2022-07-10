<?php
declare(strict_types=1);

namespace App\Validator\Constraints;

use App\Entity\Invite;
use App\Repository\InviteRepository;
use Symfony\Component\Validator\{Constraint, ConstraintValidator};

class ValidInviteValidator extends ConstraintValidator
{
    public function __construct(
        private readonly InviteRepository $inviteRepo
    ) {

    }

    /**
     * @param ValidInvite $constraint
     */
    public function validate(mixed $value, Constraint $constraint)
    {
        /** @var Invite $invite */
        if (null === $invite = $this->inviteRepo->findOneBy(['code' => $value])) {
            $this->context->buildViolation($constraint->notFoundMessage)
                ->setParameter('{{ code }}', $value)
                ->addViolation()
            ;

            return;
        }

        if (null !== $invite->getUsedBy()) {
            $this->context->buildViolation($constraint->usedMessage)
                ->setParameter('{{ code }}', $value)
                ->addViolation()
            ;
        }
    }
}
